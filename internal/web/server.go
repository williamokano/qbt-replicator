// Package web serves the HTML UI, the JSON API, health checks and metrics.
// Handlers are thin adapters over app.Service (the same façade the CLI uses).
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
	"github.com/williamokano/qbt-replicator/internal/version"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the HTTP front-end.
type Server struct {
	svc    *app.Service
	log    *slog.Logger
	base   string // base path with trailing slash
	pages  map[string]*template.Template
	handle http.Handler
	limits *keyLimiter
	fails  *failLimiter
}

// New builds the full handler, mounted at server.base_path.
func New(svc *app.Service, logger *slog.Logger) http.Handler {
	s, err := newServer(svc, logger)
	if err != nil {
		panic(err) // templates are embedded; failure is a programming error
	}
	return s
}

func newServer(svc *app.Service, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	base := strings.TrimRight(svc.Config().Server.BasePath, "/") + "/"
	s := &Server{svc: svc, log: logger, base: base, pages: map[string]*template.Template{},
		limits: newKeyLimiter(), fails: newFailLimiter()}
	funcs := template.FuncMap{
		"bytes":        humanBytes,
		"short":        func(h string) string { return h[:min(8, len(h))] },
		"pct":          func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"since":        func(t time.Time) string { return since(t) },
		"url":          func(p string) string { return base + strings.TrimPrefix(p, "/") },
		"cell":         cellLabel,
		"lower":        strings.ToLower,
		"ratio":        func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		"ts":           func(t time.Time) string { return t.Format(time.DateTime) },
		"dur":          func(d time.Duration) string { return d.Round(time.Millisecond).String() },
		"query":        url.QueryEscape,
		"inc":          func(i int) int { return i + 1 },
		"isZero":       func(t time.Time) bool { return t.IsZero() },
		"row":          func(i, v any) map[string]any { return map[string]any{"I": i, "V": v} },
		"joinList":     joinList,
		"sdur":         shortDuration,
		"envref":       func(s string) bool { return strings.Contains(s, "${") },
		"list4":        func(a, b, c, d string) []string { return []string{a, b, c, d} },
		"hasPrefix":    strings.HasPrefix,
		"notifyEvents": func() []string { return config.NotificationEvents },
		"eventLabel":   func(k string) string { return eventLabels[k] },
	}
	for _, page := range []string{"dashboard", "torrents", "plan", "events", "history", "config", "configyaml", "apikeys"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", page, err)
		}
		s.pages[page] = t
	}

	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(svc.Engine().Metrics().Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /torrents", s.torrentsPage)
	mux.HandleFunc("GET /plan", s.planPage)
	mux.HandleFunc("GET /events", s.eventsPage)
	mux.HandleFunc("GET /history", s.historyPage)
	mux.HandleFunc("GET /config", s.configPage)
	mux.HandleFunc("POST /actions/sync", s.actionSync)
	mux.HandleFunc("POST /actions/replicate", s.actionReplicate)
	mux.HandleFunc("POST /actions/unreplicate", s.actionUnreplicate)
	mux.HandleFunc("POST /actions/include", s.actionInclude)
	mux.HandleFunc("POST /config", s.configSave)
	mux.HandleFunc("GET /config/yaml", s.configYAMLPage)
	mux.HandleFunc("POST /config/form", s.configFormSave)
	mux.HandleFunc("POST /config/test-instance", s.configTestInstance)
	mux.HandleFunc("POST /config/test-notification", s.configTestNotification)
	mux.HandleFunc("GET /config/api-keys", s.apiKeysPage)
	mux.HandleFunc("POST /config/api-keys", s.apiKeyCreate)
	mux.HandleFunc("POST /config/api-keys/{id}/revoke", s.apiKeyRevoke)

	mux.HandleFunc("GET /api/v1/status", s.apiStatus)
	mux.HandleFunc("GET /api/v1/torrents", s.apiTorrents)
	mux.HandleFunc("GET /api/v1/plan", s.apiPlan)
	mux.HandleFunc("GET /api/v1/events", s.apiEvents)
	mux.HandleFunc("GET /api/v1/history", s.apiHistory)
	mux.HandleFunc("GET /api/v1/config", s.apiConfig)
	mux.HandleFunc("GET /api/v1/config/raw", s.apiConfigRaw)
	mux.HandleFunc("POST /api/v1/instances/test", s.apiTestInstance)
	mux.HandleFunc("GET /api/v1/notifications", s.apiNotifications)
	mux.HandleFunc("POST /api/v1/notifications/test", s.apiTestNotification)
	mux.HandleFunc("PUT /api/v1/config/raw", s.apiConfigSave)
	mux.HandleFunc("POST /api/v1/sync", s.apiSync)
	mux.HandleFunc("POST /api/v1/replicate", s.apiReplicate)
	mux.HandleFunc("POST /api/v1/unreplicate", s.apiUnreplicate)
	mux.HandleFunc("POST /api/v1/include", s.apiInclude)

	var h http.Handler = mux
	h = s.sameOrigin(h)
	h = s.authenticate(h)
	if base != "/" {
		inner := h
		prefix := strings.TrimRight(base, "/")
		h = http.StripPrefix(prefix, inner)
		root := http.NewServeMux()
		root.Handle(base, h)
		root.Handle("GET "+prefix, http.RedirectHandler(base, http.StatusMovedPermanently))
		h = root
	}
	s.handle = s.recoverer(s.logging(h))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handle.ServeHTTP(w, r) }

// --- middleware ---

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if strings.HasSuffix(r.URL.Path, "/healthz") || strings.HasSuffix(r.URL.Path, "/metrics") {
			return
		}
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration", time.Since(start).Round(time.Microsecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
					panic(v)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type apiKeyCtx struct{}

// apiKeyFrom returns the API key that authenticated the request, or nil
// (web UI login, or an open server).
func apiKeyFrom(ctx context.Context) *store.APIKey {
	k, _ := ctx.Value(apiKeyCtx{}).(*store.APIKey)
	return k
}

// requestKey returns the API key sent as "Authorization: Bearer …" or "X-Api-Key".
func requestKey(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// keyPath reports whether API keys are accepted for a path: the JSON API and metrics.
func keyPath(p string) bool { return strings.HasPrefix(p, "/api/") || p == "/metrics" }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter, wait time.Duration, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
	writeErr(w, http.StatusTooManyRequests, errors.New(msg))
}

// authenticate checks API keys and the web UI login. Credentials are read per
// request so changes apply immediately. Once an API key exists, the JSON API
// requires a key (or the web login) even when the login is disabled.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		if secret := requestKey(r); secret != "" {
			s.withAPIKey(w, r, secret, next)
			return
		}
		auth := s.svc.Config().Server.Auth
		if auth.Enabled() {
			u, p, ok := r.BasicAuth()
			userOK := subtle.ConstantTimeCompare([]byte(u), []byte(auth.Username)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(p), []byte(auth.Password)) == 1
			if !ok || !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="qbt-replicator", charset="UTF-8"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			has, err := s.svc.HasAPIKeys(r.Context())
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if has {
				writeErr(w, http.StatusUnauthorized, errors.New("API key required (Authorization: Bearer qbtr_…)"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withAPIKey authenticates an API key request: invalid keys count against the
// client IP, read-only keys may only read, and each key is rate limited.
func (s *Server) withAPIKey(w http.ResponseWriter, r *http.Request, secret string, next http.Handler) {
	if !keyPath(r.URL.Path) {
		writeErr(w, http.StatusUnauthorized, errors.New("API keys are accepted for /api/ and /metrics only; the web UI uses the login"))
		return
	}
	now, ip := time.Now(), clientIP(r)
	if blocked, wait := s.fails.blocked(ip, now); blocked {
		tooMany(w, wait, "too many invalid API keys from this address")
		return
	}
	k, err := s.svc.AuthenticateAPIKey(r.Context(), secret)
	if errors.Is(err, app.ErrInvalidAPIKey) {
		s.fails.fail(ip, now)
		s.log.Warn("invalid API key", "ip", ip, "path", r.URL.Path)
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if write && k.Scope != app.ScopeFull {
		writeErr(w, http.StatusForbidden, errors.New("this API key is read-only"))
		return
	}
	if ok, wait := s.limits.allow(k.ID, write, s.svc.Config().Server.API.RateLimit, now); !ok {
		tooMany(w, wait, fmt.Sprintf("rate limit exceeded for API key %q", k.Name))
		return
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyCtx{}, &k)))
}

// sameOrigin rejects cross-site POSTs (CSRF) when the browser reports an origin.
func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			src := r.Header.Get("Origin")
			if src == "" {
				src = r.Header.Get("Referer")
			}
			if src != "" && src != "null" {
				u, err := url.Parse(src)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- health ---

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	st := s.svc.CachedStatus(r.Context())
	for _, in := range st.Instances {
		if in.Healthy {
			_, _ = w.Write([]byte("ready\n"))
			return
		}
	}
	http.Error(w, "no qBittorrent instance reachable", http.StatusServiceUnavailable)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Page"] = page
	data["Version"] = version.Version
	data["Flash"] = r.URL.Query().Get("msg")
	data["FlashErr"] = r.URL.Query().Get("err")
	data["Refresh"] = r.URL.Query().Get("refresh")
	data["DryRun"] = s.svc.Config().Daemon.DryRun
	data["Configured"] = s.svc.Config().Configured()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		s.log.Error("rendering template", "page", page, "err", err)
	}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, msg string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if msg != "" {
		q.Set("msg", msg)
	}
	if to == "" {
		to = "/"
	}
	target := s.base + strings.TrimPrefix(to, "/")
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // target is base path + local path validated by returnTo
}

// returnTo validates a local return path from a form.
func returnTo(r *http.Request) string {
	p := r.FormValue("return")
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func since(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Second {
		return "just now"
	}
	return d.String() + " ago"
}

func cellLabel(c app.Cell) string {
	switch {
	case !c.Present:
		return "–"
	case c.Progress < 1:
		return fmt.Sprintf("%.0f%%", c.Progress*100)
	case c.Replica:
		return "replica"
	default:
		return "source"
	}
}

var errMissingHash = errors.New("hash is required")

// replicateFromForm parses a replication request from form values.
func replicateFromForm(r *http.Request) (replicator.ReplicateRequest, error) {
	req := replicator.ReplicateRequest{
		Hash:   strings.ToLower(strings.TrimSpace(r.FormValue("hash"))),
		From:   r.FormValue("from"),
		To:     splitList(r.Form["to"]),
		Force:  r.FormValue("force") == "on" || r.FormValue("force") == "true",
		DryRun: r.FormValue("dry_run") == "on" || r.FormValue("dry_run") == "true",
	}
	if req.Hash == "" {
		return req, errMissingHash
	}
	return req, nil
}
