package replicator

import (
	"fmt"
	"strings"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/notify"
)

// OnNotice registers the receiver of notifications (replicas added/removed,
// failures, instance health). Set it before the engine runs.
func (e *Engine) OnNotice(f func(notify.Notice)) { e.notice = f }

func (e *Engine) emit(n notify.Notice) {
	if e.notice != nil {
		e.notice(n)
	}
}

var actionVerbs = map[ActionType]string{
	ActionAdd: "Replicating", ActionReadd: "Re-adding", ActionRemove: "Removing", ActionRecheck: "Rechecking",
	ActionStop: "Stopping", ActionSyncLimits: "Syncing share limits of", ActionExclude: "Excluding", ActionInclude: "Including",
}

// noticeResults turns executed actions into notifications. Successful adds of
// one torrent to several targets become a single notice.
func (e *Engine) noticeResults(results []Result) {
	if e.notice == nil {
		return
	}
	e.stateMu.RLock()
	failures := e.memory.Failures
	e.stateMu.RUnlock()
	type group struct {
		n       notify.Notice
		targets []string
	}
	var added []*group
	byKey := map[string]*group{}
	for _, r := range results {
		a := r.Action
		n := notify.Notice{Action: string(a.Type), Instance: a.Target, Source: a.Source, Hash: a.Hash, Name: a.Name, Level: notify.LevelInfo}
		switch {
		case r.Error != "":
			n.Kind, n.Level, n.Error = config.EventActionFailed, notify.LevelError, r.Error
			n.Title = fmt.Sprintf("%s %s on %s failed", actionVerbs[a.Type], a.Name, a.Target)
			n.Text = r.Error
			if f, ok := failures[a.key()]; ok {
				n.Text += fmt.Sprintf("\nAttempt %d; retrying at %s.", f.Count, f.Until.Format("15:04"))
			}
			e.emit(n)
		case a.Type == ActionAdd:
			key := a.Hash + "|" + a.Source
			g, ok := byKey[key]
			if !ok {
				n.Kind = config.EventReplicaAdded
				g = &group{n: n}
				byKey[key] = g
				added = append(added, g)
			}
			g.targets = append(g.targets, a.Target)
		case a.Type == ActionReadd:
			n.Kind, n.Title = config.EventReplicaAdded, "Re-added "+a.Name
			n.Text = fmt.Sprintf("on %s: %s", a.Target, a.Reason)
			e.emit(n)
		case a.Type == ActionRemove:
			n.Kind, n.Title = config.EventReplicaRemoved, "Removed replica "+a.Name
			n.Text = fmt.Sprintf("from %s: %s", a.Target, a.Reason)
			e.emit(n)
		}
	}
	for _, g := range added {
		n := g.n
		n.Title = "Replicated " + n.Name
		n.Text = n.Source + " → " + strings.Join(g.targets, ", ")
		n.Instance = strings.Join(g.targets, ",")
		e.emit(n)
	}
}
