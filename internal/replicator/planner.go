package replicator

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// ActionType enumerates planned operations.
type ActionType string

// Action types.
const (
	ActionAdd        ActionType = "add"
	ActionRemove     ActionType = "remove"
	ActionReadd      ActionType = "readd"
	ActionRecheck    ActionType = "recheck"
	ActionStop       ActionType = "stop"
	ActionSyncLimits ActionType = "sync_limits"
)

// Action is one planned operation on a target instance.
type Action struct {
	Type   ActionType `json:"type"`
	Hash   string     `json:"hash"`
	Name   string     `json:"name"`
	Source string     `json:"source,omitempty"`
	Target string     `json:"target"`
	Rule   string     `json:"rule,omitempty"`
	Reason string     `json:"reason"`

	// Add/Readd parameters.
	SavePath     string   `json:"save_path,omitempty"`
	Category     string   `json:"category,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	SkipChecking bool     `json:"skip_checking,omitempty"`
	Start        bool     `json:"start,omitempty"`

	// Share limits (Add/Readd when the rule syncs limits, and SyncLimits).
	SyncLimits          bool    `json:"sync_limits,omitempty"`
	RatioLimit          float64 `json:"ratio_limit,omitempty"`
	SeedingTimeLimit    int64   `json:"seeding_time_limit,omitempty"`
	InactiveSeedingTime int64   `json:"inactive_seeding_time_limit,omitempty"`
}

func (a Action) key() string { return a.Target + "|" + a.Hash }

// String renders a one-line summary.
func (a Action) String() string {
	src := ""
	if a.Source != "" {
		src = a.Source + " -> "
	}
	return fmt.Sprintf("%s %s%s %s (%s): %s", a.Type, src, a.Target, a.Name, short(a.Hash), a.Reason)
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// Memory carries planner state between ticks (orphan grace, recheck attempts).
type Memory struct {
	Orphans  map[string]int  `json:"orphans"`
	Rechecks map[string]bool `json:"rechecks"`
}

// NewMemory returns empty planner memory.
func NewMemory() *Memory {
	return &Memory{Orphans: map[string]int{}, Rechecks: map[string]bool{}}
}

func (m *Memory) clone() *Memory {
	out := NewMemory()
	for k, v := range m.Orphans {
		out.Orphans[k] = v
	}
	for k, v := range m.Rechecks {
		out.Rechecks[k] = v
	}
	return out
}

// Plan is the planner output.
type Plan struct {
	At      time.Time `json:"at"`
	Actions []Action  `json:"actions"`
	// Notes explain decisions that were deliberately not taken.
	Notes []string `json:"notes"`
	// Memory is the updated planner memory (commit it only if the plan is executed).
	Memory *Memory `json:"-"`
}

// Compute is the pure planning function: config + snapshot + memory -> actions.
func Compute(cfg *config.Config, snap *Snapshot, mem *Memory, now time.Time) *Plan {
	if mem == nil {
		mem = NewMemory()
	}
	p := &Plan{At: now, Memory: mem.clone()}
	tag := cfg.Daemon.ReplicaTag
	seen := map[string]bool{}
	push := func(a Action) {
		if !seen[a.key()] {
			seen[a.key()] = true
			p.Actions = append(p.Actions, a)
		}
	}

	// owners: hash -> instances holding it as a non-replica.
	owners := map[string][]string{}
	for _, name := range snap.Order {
		st := snap.Instances[name]
		if st == nil || !st.Healthy {
			continue
		}
		for h, t := range st.Torrents {
			if !t.HasTag(tag) {
				owners[h] = append(owners[h], name)
			}
		}
	}

	// 1. Replicate eligible source torrents.
	for _, rule := range cfg.Rules {
		for _, srcName := range cfg.Expand(rule.Sources) {
			src := snap.Instances[srcName]
			if src == nil || !src.Healthy {
				continue
			}
			srcCfg, _ := cfg.Instance(srcName)
			for _, h := range sortedKeys(src.Torrents) {
				t := src.Torrents[h]
				if ok, _ := Eligible(cfg, rule, src, t, now); !ok {
					continue
				}
				for _, tgtName := range cfg.Expand(rule.Targets, srcName) {
					tgt := snap.Instances[tgtName]
					if tgt == nil || !tgt.Healthy {
						continue
					}
					if _, present := tgt.Torrents[h]; present {
						continue
					}
					tgtCfg, _ := cfg.Instance(tgtName)
					push(addAction(ActionAdd, cfg, rule, srcCfg, tgtCfg, t, "completed on "+srcName))
				}
			}
		}
	}

	// 2. Maintain existing replicas.
	allHealthy := snap.AllHealthy()
	activeOrphans := map[string]int{}
	for _, tgtName := range snap.Order {
		tgt := snap.Instances[tgtName]
		if tgt == nil || !tgt.Healthy {
			continue
		}
		tgtCfg, _ := cfg.Instance(tgtName)
		for _, h := range sortedKeys(tgt.Torrents) {
			r := tgt.Torrents[h]
			if !r.HasTag(tag) {
				continue
			}
			key := tgtName + "|" + h
			own := owners[h]
			if len(own) == 0 {
				if !removeOrphansFor(cfg, tgtName) {
					continue
				}
				if !allHealthy {
					p.Notes = append(p.Notes, fmt.Sprintf("orphan %s on %s kept: not every instance is reachable", r.Name, tgtName))
					continue
				}
				n := mem.Orphans[key] + 1
				activeOrphans[key] = n
				if n >= cfg.Daemon.OrphanGraceTicks {
					push(Action{Type: ActionRemove, Hash: h, Name: r.Name, Target: tgtName,
						Reason: "source torrent no longer exists on any instance (files kept)"})
				} else {
					p.Notes = append(p.Notes, fmt.Sprintf("orphan %s on %s: confirmation %d/%d", r.Name, tgtName, n, cfg.Daemon.OrphanGraceTicks))
				}
				continue
			}
			srcName := own[0]
			src := snap.Instances[srcName]
			t := src.Torrents[h]
			srcCfg, _ := cfg.Instance(srcName)
			rule, hasRule := ruleFor(cfg, srcName, tgtName)

			switch {
			case qbt.IsErrorState(r.State):
				if !p.Memory.Rechecks[key] {
					p.Memory.Rechecks[key] = true
					push(Action{Type: ActionRecheck, Hash: h, Name: r.Name, Source: srcName, Target: tgtName,
						Reason: "replica in state " + r.State})
				} else {
					p.Notes = append(p.Notes, fmt.Sprintf("replica %s on %s still %s after recheck", r.Name, tgtName, r.State))
				}
				continue
			case r.Progress < 1 && !strings.HasPrefix(r.State, "checking") && !qbt.IsStoppedState(r.State) && r.State != qbt.StateMoving:
				push(Action{Type: ActionStop, Hash: h, Name: r.Name, Source: srcName, Target: tgtName,
					Reason: fmt.Sprintf("replica incomplete (%.1f%%), stopping to avoid downloading", r.Progress*100)})
				continue
			}
			delete(p.Memory.Rechecks, key)

			if !hasRule {
				continue
			}
			settled := qbt.IsSeedingState(t.State) && t.Progress >= 1
			if rule.FollowRelocation && settled {
				want := tgtCfg.FromCanonical(srcCfg.ToCanonical(t.SavePath))
				if !config.SamePath(want, r.SavePath) {
					a := addAction(ActionReadd, cfg, rule, srcCfg, tgtCfg, t,
						fmt.Sprintf("source moved to %s (replica at %s)", t.SavePath, r.SavePath))
					push(a)
					continue
				}
			}
			if rule.SyncShareLimits && (r.RatioLimit != t.RatioLimit || r.SeedingTimeLimit != t.SeedingTimeLimit || r.InactiveSeedingTime != t.InactiveSeedingTime) {
				push(Action{Type: ActionSyncLimits, Hash: h, Name: r.Name, Source: srcName, Target: tgtName, Rule: rule.Name,
					Reason: "share limits differ from source", SyncLimits: true, RatioLimit: t.RatioLimit,
					SeedingTimeLimit: t.SeedingTimeLimit, InactiveSeedingTime: t.InactiveSeedingTime})
			}
		}
	}
	// Orphans not seen this tick reset their counter; untouched counters survive unhealthy ticks.
	if allHealthy {
		p.Memory.Orphans = activeOrphans
	} else {
		for k, v := range activeOrphans {
			p.Memory.Orphans[k] = v
		}
	}
	return p
}

func addAction(typ ActionType, cfg *config.Config, rule config.RuleConfig, srcCfg, tgtCfg config.InstanceConfig, t qbt.Torrent, reason string) Action {
	cat := rule.Add.Category
	if cat == config.CategoryInherit {
		cat = t.Category
	}
	tags := append([]string{cfg.Daemon.ReplicaTag}, rule.Add.ExtraTags...)
	a := Action{
		Type: typ, Hash: t.Hash, Name: t.Name, Source: srcCfg.Name, Target: tgtCfg.Name, Rule: rule.Name,
		Reason:       reason,
		SavePath:     tgtCfg.FromCanonical(srcCfg.ToCanonical(t.SavePath)),
		Category:     cat,
		Tags:         tags,
		SkipChecking: rule.Add.SkipChecking,
		Start:        rule.Add.Start,
	}
	if rule.SyncShareLimits {
		a.SyncLimits = true
		a.RatioLimit, a.SeedingTimeLimit, a.InactiveSeedingTime = t.RatioLimit, t.SeedingTimeLimit, t.InactiveSeedingTime
	}
	return a
}

// Eligible reports whether torrent t on src should be replicated by rule, and why not.
func Eligible(cfg *config.Config, rule config.RuleConfig, src *InstanceState, t qbt.Torrent, now time.Time) (bool, string) {
	if t.HasTag(cfg.Daemon.ReplicaTag) {
		return false, "is a replica"
	}
	if t.Progress < 1 || !qbt.IsSeedingState(t.State) {
		return false, "not completed/settled (state " + t.State + ")"
	}
	done := t.CompletedAt()
	if done.IsZero() && t.AddedOn > 0 {
		done = time.Unix(t.AddedOn, 0)
	}
	if !done.IsZero() && now.Sub(done) < rule.SettleDelay {
		return false, "within settle delay"
	}
	f := rule.Filter
	if len(f.Categories) > 0 && !slices.Contains(f.Categories, t.Category) {
		return false, "category not selected"
	}
	tags := t.Tags()
	if len(f.Tags) > 0 && !slices.ContainsFunc(f.Tags, func(x string) bool { return slices.Contains(tags, x) }) {
		return false, "tag not selected"
	}
	if slices.ContainsFunc(f.ExcludeTags, func(x string) bool { return slices.Contains(tags, x) }) {
		return false, "excluded tag"
	}
	if len(f.Trackers) > 0 && !slices.ContainsFunc(f.Trackers, func(x string) bool {
		return x != "" && strings.Contains(strings.ToLower(t.Tracker), strings.ToLower(x))
	}) {
		return false, "tracker not selected"
	}
	size := t.TotalSize
	if size == 0 {
		size = t.Size
	}
	if f.MinSizeBytes > 0 && size < f.MinSizeBytes {
		return false, "smaller than min_size"
	}
	if f.MinRatio > 0 && t.Ratio < f.MinRatio {
		return false, "ratio below min_ratio"
	}
	if !f.IncludePrivate {
		priv, known := src.Private[t.Hash]
		if !known && t.Private != nil {
			priv, known = *t.Private, true
		}
		if !known || priv {
			return false, "private torrent (include_private is false)"
		}
	}
	return true, ""
}

// ruleFor finds the first rule replicating src -> tgt.
func ruleFor(cfg *config.Config, src, tgt string) (config.RuleConfig, bool) {
	for _, r := range cfg.Rules {
		if slices.Contains(cfg.Expand(r.Sources), src) && slices.Contains(cfg.Expand(r.Targets, src), tgt) {
			return r, true
		}
	}
	return config.RuleConfig{}, false
}

func removeOrphansFor(cfg *config.Config, tgt string) bool {
	for _, r := range cfg.Rules {
		if r.RemoveOrphans && slices.Contains(cfg.Expand(r.Targets), tgt) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]qbt.Torrent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
