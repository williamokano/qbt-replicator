package web

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/store"
)

const historyPageSize = 100

func historyFilter(r *http.Request) store.ActionFilter {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = historyPageSize
	}
	f := store.ActionFilter{
		Hash: q.Get("hash"), Search: q.Get("search"), Instance: q.Get("instance"),
		Type: q.Get("type"), Result: q.Get("result"), BeforeID: before, Limit: limit,
	}
	f.Since = parseSince(q.Get("since"), time.Now())
	return f
}

// parseSince accepts an RFC 3339 time or a duration back from now ("24h", "7d").
func parseSince(s string, now time.Time) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if d, err := config.ParseDays(s); err == nil && d > 0 {
		return now.Add(-d)
	}
	return time.Time{}
}

func eventFilter(r *http.Request) store.EventFilter {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = historyPageSize
	}
	return store.EventFilter{Hash: q.Get("hash"), Instance: q.Get("instance"), Level: q.Get("level"), BeforeID: before, Limit: limit}
}

// olderLink returns the query string of the next (older) page, or "" when this
// page was not full.
func olderLink(r *http.Request, n, limit int, lastID int64) string {
	if n < limit || lastID <= 1 {
		return ""
	}
	q := url.Values{}
	for k, v := range r.URL.Query() {
		q[k] = v
	}
	q.Set("before", strconv.FormatInt(lastID, 10))
	return "?" + q.Encode()
}

// actionTypes and actionResults feed the history filters.
var (
	actionTypes   = []string{"add", "readd", "remove", "recheck", "stop", "sync_limits", "exclude", "include"}
	actionResults = []string{"ok", "error"}
)

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := historyFilter(r)
	data := map[string]any{"Filter": f, "Instances": s.svc.Config().InstanceNames(),
		"Types": actionTypes, "Results": actionResults}
	acts, err := s.svc.History(ctx, f)
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Actions"] = acts
	if len(acts) > 0 {
		data["Older"] = olderLink(r, len(acts), f.Limit, acts[len(acts)-1].ID)
	}
	if f.Hash != "" {
		// Torrent timeline: its actions plus the activity log entries about it.
		evs, _ := s.svc.EventHistory(ctx, store.EventFilter{Hash: f.Hash, Limit: 200})
		data["Events"] = evs
		for _, a := range acts {
			data["TorrentName"] = a.Name
			break
		}
		for _, e := range evs {
			if data["TorrentName"] == nil && e.Name != "" {
				data["TorrentName"] = e.Name
			}
		}
	} else {
		now := time.Now()
		day, _ := s.svc.HistoryStats(ctx, now.Add(-24*time.Hour))
		week, _ := s.svc.HistoryStats(ctx, now.Add(-7*24*time.Hour))
		data["Day"], data["Week"] = day, week
	}
	data["Filtered"] = f.Hash != "" || f.Search != "" || f.Instance != "" || f.Type != "" || f.Result != ""
	s.render(w, r, "history", data)
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	acts, err := s.svc.History(r.Context(), historyFilter(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, acts)
}

// events returns persisted events, falling back to the in-memory log.
func (s *Server) events(r *http.Request, f store.EventFilter) []events.Event {
	evs, err := s.svc.EventHistory(r.Context(), f)
	if err != nil {
		s.log.Warn("reading events from the database", "err", err)
		return s.svc.Events(f.Limit)
	}
	return evs
}
