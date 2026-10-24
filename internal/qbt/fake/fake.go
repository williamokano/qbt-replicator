// Package fake is an in-memory qBittorrent WebUI used in tests. It emulates the
// subset of the API qbt-replicator relies on, in either the 4.x or 5.x dialect.
//
// Exported ".torrent" files are JSON-encoded Meta values, which is enough for
// the replicator because it treats torrent files as opaque bytes.
package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// Meta is the fake .torrent payload.
type Meta struct {
	Hash    string     `json:"hash"`
	Name    string     `json:"name"`
	Private bool       `json:"private"`
	Files   []qbt.File `json:"files"`
}

// Server is a fake qBittorrent instance.
type Server struct {
	*httptest.Server

	mu         sync.Mutex
	v5         bool
	user, pass string
	sid        string
	torrents   map[string]*Entry
	categories map[string]qbt.Category
	calls      []string

	// NoExport makes /torrents/export return 404 (qBittorrent < 4.5).
	NoExport bool
	// Down makes every request fail with 502.
	Down bool
	// DeletedFiles counts delete calls that asked for file deletion (must stay 0).
	DeletedFiles int
	// HasData reports whether the shared storage contains the torrent data;
	// used to compute progress after add/recheck. Defaults to true.
	HasData func(hash string) bool
	// ConnectionStatus is reported by /transfer/info.
	ConnectionStatus string
	// APIKey, when set, is accepted as "Authorization: Bearer <key>" instead of a session.
	APIKey string
}

// Entry is a torrent held by the fake.
type Entry struct {
	Torrent qbt.Torrent
	Files   []qbt.File
	Private bool
	// AddParams records the multipart fields used to add it.
	AddParams map[string]string
	// SeedMode is set for skip_checking torrents added stopped.
	SeedMode bool
}

// New starts a fake server. v5 selects the 5.x dialect.
func New(v5 bool, user, pass string) *Server {
	s := &Server{
		v5: v5, user: user, pass: pass,
		torrents:         map[string]*Entry{},
		categories:       map[string]qbt.Category{},
		ConnectionStatus: "connected",
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Seed adds a complete, seeding torrent (as if downloaded by this instance).
func (s *Server) Seed(t qbt.Torrent, files []qbt.File, private bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.State == "" {
		t.State = qbt.StateStalledUP
	}
	if t.Progress == 0 && qbt.IsSeedingState(t.State) {
		t.Progress = 1
	}
	if t.CompletionOn == 0 && t.Progress == 1 {
		t.CompletionOn = time.Now().Add(-time.Hour).Unix()
	}
	if t.ContentPath == "" {
		t.ContentPath = strings.TrimRight(t.SavePath, "/") + "/" + t.Name
	}
	if files == nil {
		idx := 0
		files = []qbt.File{{Index: &idx, Name: t.Name, Size: t.Size, Progress: t.Progress, Priority: 1}}
	}
	if s.v5 {
		p := private
		t.Private = &p
	}
	s.torrents[t.Hash] = &Entry{Torrent: t, Files: files, Private: private}
}

// Get returns a copy of a torrent entry.
func (s *Server) Get(hash string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.torrents[hash]
	if !ok {
		return Entry{}, false
	}
	cp := *e
	cp.Files = slices.Clone(e.Files)
	return cp, true
}

// Update mutates a torrent in place.
func (s *Server) Update(hash string, fn func(e *Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.torrents[hash]; ok {
		fn(e)
	}
}

// Delete removes a torrent (e.g. simulating Sonarr removing it).
func (s *Server) Delete(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.torrents, hash)
}

// Hashes lists held torrent hashes.
func (s *Server) Hashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for h := range s.torrents {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

// Calls returns the endpoints invoked so far.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// ExpireSession invalidates the current SID (forces re-login).
func (s *Server) ExpireSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sid = ""
}

func (s *Server) hasData(hash string) bool {
	if s.HasData == nil {
		return true
	}
	return s.HasData(hash)
}

func (s *Server) stoppedState() string {
	if s.v5 {
		return qbt.StateStoppedUP
	}
	return qbt.StatePausedUP
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	s.calls = append(s.calls, ep)
	if s.Down {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	if ep == "auth/login" {
		_ = r.ParseForm()
		if r.PostForm.Get("username") != s.user || r.PostForm.Get("password") != s.pass {
			if s.v5 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			} else {
				_, _ = io.WriteString(w, "Fails.")
			}
			return
		}
		s.sid = strconv.FormatInt(time.Now().UnixNano(), 36)
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: s.sid, Path: "/"})
		if s.v5 {
			w.WriteHeader(http.StatusNoContent) // qBittorrent 5.x
			return
		}
		_, _ = io.WriteString(w, "Ok.")
		return
	}
	byKey := s.APIKey != "" && r.Header.Get("Authorization") == "Bearer "+s.APIKey
	if !byKey && (s.user != "" || s.APIKey != "") {
		c, err := r.Cookie("SID")
		if err != nil || s.sid == "" || c.Value != s.sid {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	switch ep {
	case "app/webapiVersion":
		if s.v5 {
			_, _ = io.WriteString(w, "2.16.2")
		} else {
			_, _ = io.WriteString(w, "2.9.3")
		}
	case "app/version":
		if s.v5 {
			_, _ = io.WriteString(w, "v5.3.0")
		} else {
			_, _ = io.WriteString(w, "v4.6.7")
		}
	case "app/preferences":
		writeJSON(w, map[string]any{"listen_port": 51413})
	case "transfer/info":
		var up int64
		for _, e := range s.torrents {
			up += e.Torrent.UpSpeed
		}
		writeJSON(w, qbt.TransferInfo{UpSpeed: up, ConnectionStatus: s.ConnectionStatus})
	case "torrents/info":
		s.info(w, r)
	case "torrents/files":
		e, ok := s.torrents[r.URL.Query().Get("hash")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, e.Files)
	case "torrents/properties":
		e, ok := s.torrents[r.URL.Query().Get("hash")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"is_private": e.Private, "save_path": e.Torrent.SavePath})
	case "torrents/export":
		e, ok := s.torrents[r.URL.Query().Get("hash")]
		if !ok || s.NoExport {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, Meta{Hash: e.Torrent.Hash, Name: e.Torrent.Name, Private: e.Private, Files: originalFiles(e)})
	case "torrents/add":
		s.add(w, r)
	case "torrents/delete":
		_ = r.ParseForm()
		if r.PostForm.Get("deleteFiles") != "false" {
			s.DeletedFiles++
		}
		for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
			delete(s.torrents, h)
		}
	case "torrents/start", "torrents/resume", "torrents/stop", "torrents/pause":
		if s.v5 != (ep == "torrents/start" || ep == "torrents/stop") {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		start := ep == "torrents/start" || ep == "torrents/resume"
		for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
			if e, ok := s.torrents[h]; ok {
				if start && e.SeedMode {
					e.SeedMode = false
					s.recompute(e)
				}
				switch {
				case start && e.Torrent.Progress >= 1:
					e.Torrent.State = qbt.StateStalledUP
				case start:
					e.Torrent.State = "downloading"
				case e.Torrent.Progress >= 1:
					e.Torrent.State = s.stoppedState()
				default:
					e.Torrent.State = strings.Replace(s.stoppedState(), "UP", "DL", 1)
				}
			}
		}
	case "torrents/recheck":
		_ = r.ParseForm()
		for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
			if e, ok := s.torrents[h]; ok {
				s.recompute(e)
			}
		}
	case "torrents/filePrio":
		_ = r.ParseForm()
		e, ok := s.torrents[r.PostForm.Get("hash")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		prio, _ := strconv.Atoi(r.PostForm.Get("priority"))
		for _, id := range strings.Split(r.PostForm.Get("id"), "|") {
			i, _ := strconv.Atoi(id)
			for j := range e.Files {
				if e.Files[j].Idx() == i {
					e.Files[j].Priority = prio
				}
			}
		}
	case "torrents/renameFile", "torrents/renameFolder":
		_ = r.ParseForm()
		e, ok := s.torrents[r.PostForm.Get("hash")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		oldP, newP := r.PostForm.Get("oldPath"), r.PostForm.Get("newPath")
		found := false
		for j := range e.Files {
			n := e.Files[j].Name
			if ep == "torrents/renameFile" && n == oldP {
				e.Files[j].Name, found = newP, true
			}
			if ep == "torrents/renameFolder" && strings.HasPrefix(n, oldP+"/") {
				e.Files[j].Name, found = newP+strings.TrimPrefix(n, oldP), true
			}
		}
		if !found {
			http.Error(w, "invalid path", http.StatusConflict)
		}
	case "torrents/setShareLimits":
		_ = r.ParseForm()
		if e, ok := s.torrents[r.PostForm.Get("hashes")]; ok {
			e.Torrent.RatioLimit, _ = strconv.ParseFloat(r.PostForm.Get("ratioLimit"), 64)
			e.Torrent.SeedingTimeLimit, _ = strconv.ParseInt(r.PostForm.Get("seedingTimeLimit"), 10, 64)
		}
	case "torrents/addTags", "torrents/removeTags":
		_ = r.ParseForm()
		tags := strings.Split(r.PostForm.Get("tags"), ",")
		for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
			e, ok := s.torrents[h]
			if !ok {
				continue
			}
			cur := e.Torrent.Tags()
			for _, tg := range tags {
				tg = strings.TrimSpace(tg)
				has := slices.Contains(cur, tg)
				if ep == "torrents/addTags" && !has && tg != "" {
					cur = append(cur, tg)
				}
				if ep == "torrents/removeTags" && has {
					cur = slices.DeleteFunc(cur, func(x string) bool { return x == tg })
				}
			}
			e.Torrent.RawTags = strings.Join(cur, ", ")
		}
	case "torrents/categories":
		writeJSON(w, s.categories)
	case "torrents/createCategory":
		_ = r.ParseForm()
		n := r.PostForm.Get("category")
		s.categories[n] = qbt.Category{Name: n, SavePath: r.PostForm.Get("savePath")}
	default:
		http.NotFound(w, r)
	}
}

// originalFiles reconstructs the file list as stored in the .torrent (before renames).
func originalFiles(e *Entry) []qbt.File {
	if orig, ok := e.AddParams["_orig"]; ok {
		var fs []qbt.File
		_ = json.Unmarshal([]byte(orig), &fs)
		return fs
	}
	out := make([]qbt.File, len(e.Files))
	for i, f := range e.Files {
		f.Priority = 1
		out[i] = f
	}
	return out
}

func (s *Server) recompute(e *Entry) {
	if s.hasData(e.Torrent.Hash) {
		e.Torrent.Progress = 1
	} else {
		e.Torrent.Progress = 0
	}
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	var filter []string
	if h := r.URL.Query().Get("hashes"); h != "" {
		filter = strings.Split(h, "|")
	}
	out := []qbt.Torrent{}
	for _, e := range s.torrents {
		if filter != nil && !slices.Contains(filter, e.Torrent.Hash) {
			continue
		}
		out = append(out, e.Torrent)
	}
	slices.SortFunc(out, func(a, b qbt.Torrent) int { return strings.Compare(a.Hash, b.Hash) })
	writeJSON(w, out)
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("torrents")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	var m Meta
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		http.Error(w, "invalid torrent", http.StatusUnsupportedMediaType)
		return
	}
	if _, exists := s.torrents[m.Hash]; exists {
		if s.v5 {
			http.Error(w, "Conflict", http.StatusConflict)
		} else {
			_, _ = io.WriteString(w, "Fails.")
		}
		return
	}
	params := map[string]string{}
	for k, v := range r.MultipartForm.Value {
		params[k] = v[0]
	}
	orig, _ := json.Marshal(m.Files)
	params["_orig"] = string(orig)
	stopped := params["stopped"] == "true"
	if !s.v5 {
		stopped = params["paused"] == "true"
	}
	files := slices.Clone(m.Files)
	var size int64
	for i := range files {
		files[i].Priority = 1
		size += files[i].Size
	}
	t := qbt.Torrent{
		Hash: m.Hash, Name: m.Name, Size: size, TotalSize: size,
		SavePath: params["savepath"], Category: params["category"],
		RawTags: strings.ReplaceAll(params["tags"], ",", ", "),
		AddedOn: time.Now().Unix(), ContentPath: strings.TrimRight(params["savepath"], "/") + "/" + m.Name,
		RatioLimit: -2, SeedingTimeLimit: -2,
	}
	if s.v5 {
		p := m.Private
		t.Private = &p
	}
	e := &Entry{Torrent: t, Files: files, Private: m.Private, AddParams: params}
	switch {
	case params["skip_checking"] == "true" && stopped:
		// Like libtorrent: seed mode only applies once started; 0% while stopped.
		e.SeedMode = true
	case params["skip_checking"] == "true":
		s.recompute(e)
	default:
		s.recompute(e)
	}
	if e.Torrent.Progress >= 1 {
		e.Torrent.CompletionOn = time.Now().Unix()
	}
	switch {
	case stopped && e.Torrent.Progress >= 1:
		e.Torrent.State = s.stoppedState()
	case stopped:
		e.Torrent.State = strings.Replace(s.stoppedState(), "UP", "DL", 1)
	case e.Torrent.Progress >= 1:
		e.Torrent.State = qbt.StateStalledUP
	default:
		e.Torrent.State = "downloading"
	}
	s.torrents[m.Hash] = e
	if s.v5 {
		writeJSON(w, map[string]any{"added_torrent_ids": []string{m.Hash}, "failure_count": 0, "pending_count": 0, "success_count": 1})
		return
	}
	_, _ = io.WriteString(w, "Ok.")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(fmt.Sprintf("fake: encoding json: %v", err))
	}
}
