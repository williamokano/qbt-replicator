package web

import (
	"net/http"
	"strings"

	"github.com/williamokano/qbt-replicator/internal/app"
)

// API keys are managed only from the web UI (behind the login) and the local
// CLI; these routes are outside /api/, so API keys cannot manage keys.

func (s *Server) renderAPIKeys(w http.ResponseWriter, r *http.Request, data map[string]any, status int) {
	keys, err := s.svc.APIKeys(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Keys"] = keys
	data["DocsURL"] = DocsURL
	data["RateLimit"] = s.svc.Config().Server.API.RateLimit
	data["LoginEnabled"] = s.svc.Config().Server.Auth.Enabled()
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "apikeys", data)
}

func (s *Server) apiKeysPage(w http.ResponseWriter, r *http.Request) {
	s.renderAPIKeys(w, r, map[string]any{}, http.StatusOK)
}

// apiKeyCreate shows the new key once, in the response page (never in a URL).
func (s *Server) apiKeyCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	scope := app.ScopeRead
	if r.PostForm.Get("scope") == app.ScopeFull {
		scope = app.ScopeFull
	}
	k, err := s.svc.CreateAPIKey(r.Context(), r.PostForm.Get("name"), scope)
	if err != nil {
		s.renderAPIKeys(w, r, map[string]any{"CreateErr": err.Error(), "Name": r.PostForm.Get("name")}, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderAPIKeys(w, r, map[string]any{"Created": k}, http.StatusOK)
}

func (s *Server) apiKeyRevoke(w http.ResponseWriter, r *http.Request) {
	k, err := s.svc.RevokeAPIKey(r.Context(), r.PathValue("id"))
	msg := ""
	if err == nil {
		msg = "API key " + strings.TrimSpace(k.Name) + " revoked."
	}
	s.redirect(w, r, "/config/api-keys", msg, err)
}
