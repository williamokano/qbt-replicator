package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/replicator"
)

// --- HTML pages ---

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	var st app.Status
	if r.URL.Query().Get("fresh") != "" {
		st = s.svc.Status(r.Context())
	} else {
		st = s.svc.CachedStatus(r.Context())
	}
	s.render(w, r, "dashboard", map[string]any{"Status": st, "Events": s.svc.Events(10)})
}

func torrentFilter(r *http.Request) app.TorrentFilter {
	q := r.URL.Query()
	return app.TorrentFilter{
		Instance:  q.Get("instance"),
		Search:    q.Get("search"),
		Category:  q.Get("category"),
		Completed: q.Get("completed") == "on" || q.Get("completed") == "true",
		Fresh:     q.Get("fresh") != "",
	}
}

func (s *Server) torrentsPage(w http.ResponseWriter, r *http.Request) {
	f := torrentFilter(r)
	rows := s.svc.Torrents(r.Context(), f)
	q := r.URL.Query()
	q.Del("fresh")
	s.render(w, r, "torrents", map[string]any{
		"Rows": rows, "Instances": s.svc.Config().InstanceNames(), "Filter": f,
		"Categories": s.svc.Categories(r.Context()), "Query": q.Encode(),
		"Return": r.URL.RequestURI(),
	})
}

func (s *Server) planPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "plan", map[string]any{"Plan": s.svc.Plan(r.Context())})
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	f := eventFilter(r)
	evs := s.events(r, f)
	data := map[string]any{"Events": evs, "Filter": f, "Instances": s.svc.Config().InstanceNames()}
	if len(evs) > 0 {
		data["Older"] = olderLink(r, len(evs), f.Limit, int64(evs[len(evs)-1].ID)) //nolint:gosec // database ids fit in int64
	}
	s.render(w, r, "events", data)
}

// DocsURL is the configuration reference linked from the UI.
const DocsURL = "https://github.com/williamokano/qbt-replicator/blob/main/docs/configuration.md"

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	form, err := s.svc.RawConfigStruct()
	s.renderConfigForm(w, r, form, err, nil, http.StatusOK)
}

// renderConfigForm shows the form editor for cfg (the file as written, or the
// rejected submission after a failed save).
func (s *Server) renderConfigForm(w http.ResponseWriter, r *http.Request, cfg *config.Config, loadErr, saveErr error, status int) {
	data := map[string]any{
		"DocsURL":       DocsURL,
		"Path":          s.svc.Config().Path,
		"Form":          cfg,
		"Overrides":     config.EnvOverrides(),
		"BlankInstance": config.InstanceConfig{},
		"BlankRule": config.RuleConfig{
			Targets: []string{config.Wildcard}, SettleDelay: 2 * time.Minute,
			Add:           config.AddConfig{Category: config.CategoryInherit, SkipChecking: true, Start: true},
			RemoveOrphans: true, FollowRelocation: true,
		},
	}
	if cfg != nil {
		data["Names"] = cfg.InstanceNames()
	}
	if loadErr != nil {
		data["LoadErr"] = loadErr.Error()
	}
	if saveErr != nil {
		data["SaveErr"] = saveErr.Error()
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "config", data)
}

func (s *Server) configFormSave(w http.ResponseWriter, r *http.Request) {
	base, err := s.svc.RawConfigStruct()
	if err != nil {
		s.renderConfigForm(w, r, nil, err, nil, http.StatusInternalServerError)
		return
	}
	cfg, err := configFromForm(r, base)
	if err == nil {
		var res app.SaveResult
		if res, err = s.svc.SaveConfigStruct(cfg); err == nil {
			s.redirect(w, r, "/config", saveMessage(res), nil)
			return
		}
	}
	s.renderConfigForm(w, r, cfg, nil, err, http.StatusUnprocessableEntity)
}

// configTestInstance tests one instance row of the (unsaved) config form.
func (s *Server) configTestInstance(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rows := formRows(r.PostForm, "inst")
	if len(rows) != 1 {
		writeErr(w, http.StatusBadRequest, errors.New("send exactly one instance row"))
		return
	}
	old := map[string]config.InstanceConfig{}
	if base, err := s.svc.RawConfigStruct(); err == nil {
		for _, in := range base.Instances {
			old[in.Name] = in
		}
	}
	var errs formErrors
	in := instanceFromForm(r.PostForm, rows[0], old, &errs)
	if len(errs) > 0 {
		writeJSON(w, http.StatusOK, app.InstanceTest{Message: strings.Join(errs, "; ")})
		return
	}
	writeJSON(w, http.StatusOK, s.svc.TestInstance(r.Context(), in))
}

type testInstanceBody struct {
	Name string `json:"name"`
	config.InstanceConfig
}

// apiTestInstance tests a saved instance ({"name": "qbt-1"}) or an unsaved
// one given in full ({"url": ..., "username": ..., "password": ...}).
func (s *Server) apiTestInstance(w http.ResponseWriter, r *http.Request) {
	var b testInstanceBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	in := b.InstanceConfig
	if in.URL == "" {
		saved, ok := s.svc.Config().Instance(b.Name)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("unknown instance %q", b.Name))
			return
		}
		in = saved
	}
	writeJSON(w, http.StatusOK, s.svc.TestInstance(r.Context(), in))
}

func saveMessage(res app.SaveResult) string {
	msg := "Configuration saved and applied."
	if len(res.RestartRequired) > 0 {
		msg += " Restart the container for: " + strings.Join(res.RestartRequired, ", ") + "."
	}
	return msg
}

func (s *Server) configYAMLPage(w http.ResponseWriter, r *http.Request) {
	s.renderConfigYAML(w, r, "", nil, http.StatusOK)
}

// renderConfigYAML shows the advanced editor; content overrides the file (after a failed save).
func (s *Server) renderConfigYAML(w http.ResponseWriter, r *http.Request, content string, saveErr error, status int) {
	file, err := s.svc.RawConfig()
	if content == "" {
		content = file.Content
	}
	data := map[string]any{"DocsURL": DocsURL, "File": file, "Content": content, "FileErr": err}
	if saveErr != nil {
		data["SaveErr"] = saveErr.Error()
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "configyaml", data)
}

func (s *Server) configSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirect(w, r, "/config/yaml", "", err)
		return
	}
	content := r.PostForm.Get("content")
	res, err := s.svc.SaveConfig(content)
	if err != nil {
		s.renderConfigYAML(w, r, content, err, http.StatusUnprocessableEntity)
		return
	}
	s.redirect(w, r, "/config/yaml", saveMessage(res), nil)
}

// --- HTML form actions ---

func (s *Server) actionSync(w http.ResponseWriter, r *http.Request) {
	dry := r.FormValue("dry_run") == "on" || r.FormValue("dry_run") == "true"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		s.svc.SyncOnce(ctx, dry)
	}()
	msg := "Sync started; see the activity log."
	if dry {
		msg = "Dry-run sync started; see the activity log."
	}
	s.redirect(w, r, returnTo(r), msg, nil)
}

func (s *Server) actionReplicate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	req, err := replicateFromForm(r)
	if err != nil {
		s.redirect(w, r, returnTo(r), "", err)
		return
	}
	res, err := s.svc.Replicate(r.Context(), req)
	s.redirect(w, r, returnTo(r), summarize("replicate", res), firstErr(res, err))
}

func (s *Server) actionUnreplicate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	hash := strings.ToLower(strings.TrimSpace(r.FormValue("hash")))
	if hash == "" {
		s.redirect(w, r, returnTo(r), "", errMissingHash)
		return
	}
	exclude := r.FormValue("exclude") == "true"
	res, err := s.svc.Unreplicate(r.Context(), replicator.UnreplicateRequest{
		Hash: hash, On: splitList(r.Form["on"]), Exclude: exclude,
	})
	what := "unreplicate"
	if exclude {
		what = "remove & exclude"
	}
	s.redirect(w, r, returnTo(r), summarize(what, res), firstErr(res, err))
}

func (s *Server) actionInclude(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	hash := strings.ToLower(strings.TrimSpace(r.FormValue("hash")))
	if hash == "" {
		s.redirect(w, r, returnTo(r), "", errMissingHash)
		return
	}
	res, err := s.svc.Include(r.Context(), hash, false)
	s.redirect(w, r, returnTo(r), summarize("include", res), firstErr(res, err))
}

func summarize(what string, res []replicator.Result) string {
	if len(res) == 0 {
		return what + ": nothing to do"
	}
	var targets []string
	for _, r := range res {
		targets = append(targets, r.Action.Target)
	}
	return fmt.Sprintf("%s ok on %s", what, strings.Join(targets, ", "))
}

func firstErr(res []replicator.Result, err error) error {
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Error != "" {
			return fmt.Errorf("%s on %s: %s", r.Action.Type, r.Action.Target, r.Error)
		}
	}
	return nil
}

// --- JSON API ---

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("fresh") != "" {
		writeJSON(w, http.StatusOK, s.svc.Status(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, s.svc.CachedStatus(r.Context()))
}

func (s *Server) apiTorrents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Torrents(r.Context(), torrentFilter(r)))
}

func (s *Server) apiPlan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Plan(r.Context()))
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.events(r, eventFilter(r)))
}

func (s *Server) apiConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.ConfigRedacted())
}

func (s *Server) apiConfigRaw(w http.ResponseWriter, _ *http.Request) {
	file, err := s.svc.RawConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write([]byte(file.Content))
}

// apiConfigSave replaces the config file with the request body (YAML).
func (s *Server) apiConfigSave(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.SaveConfig(string(body))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if res.Saved {
			status = http.StatusInternalServerError
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type syncBody struct {
	DryRun bool `json:"dry_run"`
}

func (s *Server) apiSync(w http.ResponseWriter, r *http.Request) {
	var b syncBody
	if err := decodeOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if r.URL.Query().Get("dry_run") == "true" {
		b.DryRun = true
	}
	writeJSON(w, http.StatusOK, s.svc.SyncOnce(r.Context(), b.DryRun))
}

type replicateBody struct {
	Hash   string   `json:"hash"`
	From   string   `json:"from"`
	To     []string `json:"to"`
	Force  bool     `json:"force"`
	DryRun bool     `json:"dry_run"`
}

func (s *Server) apiReplicate(w http.ResponseWriter, r *http.Request) {
	var b replicateBody
	if err := decodeOptional(r, &b); err != nil || b.Hash == "" {
		if err == nil {
			err = errMissingHash
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.Replicate(r.Context(), replicator.ReplicateRequest{
		Hash: strings.ToLower(b.Hash), From: b.From, To: b.To, Force: b.Force, DryRun: b.DryRun,
	})
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type unreplicateBody struct {
	Hash    string   `json:"hash"`
	On      []string `json:"on"`
	Exclude bool     `json:"exclude"`
	DryRun  bool     `json:"dry_run"`
}

func (s *Server) apiUnreplicate(w http.ResponseWriter, r *http.Request) {
	var b unreplicateBody
	if err := decodeOptional(r, &b); err != nil || b.Hash == "" {
		if err == nil {
			err = errMissingHash
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.Unreplicate(r.Context(), replicator.UnreplicateRequest{
		Hash: strings.ToLower(b.Hash), On: b.On, Exclude: b.Exclude, DryRun: b.DryRun,
	})
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type includeBody struct {
	Hash   string `json:"hash"`
	DryRun bool   `json:"dry_run"`
}

func (s *Server) apiInclude(w http.ResponseWriter, r *http.Request) {
	var b includeBody
	if err := decodeOptional(r, &b); err != nil || b.Hash == "" {
		if err == nil {
			err = errMissingHash
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.Include(r.Context(), strings.ToLower(b.Hash), b.DryRun)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// decodeOptional decodes a JSON body if present.
func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
