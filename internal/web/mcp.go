package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/backend"
	"github.com/williamokano/qbt-replicator/internal/mcpserver"
)

const mcpPath = "/mcp"

// mcpHandler serves MCP over streamable HTTP. It is stateless: every request
// gets a server built for its credentials, so a read-only key only sees the
// read tools and each tool call takes a token from that key's bucket.
func (s *Server) mcpHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		opts := mcpserver.Options{}
		if k := apiKeyFrom(r.Context()); k != nil {
			opts.ReadOnly = k.Scope != app.ScopeFull
			id, name := k.ID, k.Name
			opts.Limit = func(_ context.Context, write bool) error {
				if ok, wait := s.limits.allow(id, write, s.svc.Config().Server.API.RateLimit, time.Now()); !ok {
					return fmt.Errorf("rate limit exceeded for API key %q, retry in %s", name, wait.Round(time.Second))
				}
				return nil
			}
		}
		return mcpserver.New(backend.Local(s.svc), opts)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		Logger:       s.log,
		// Authentication is required (API key or login); the DNS-rebinding check
		// would reject reverse proxies on the same host.
		DisableLocalhostProtection: true,
	})
}

// WhoAmI describes the credentials of a request.
type WhoAmI struct {
	Auth  string `json:"auth"`           // api_key, login or none
	Name  string `json:"name,omitempty"` // API key name
	Scope string `json:"scope"`          // read or full
}

func (s *Server) apiWhoAmI(w http.ResponseWriter, r *http.Request) {
	switch k := apiKeyFrom(r.Context()); {
	case k != nil:
		writeJSON(w, http.StatusOK, WhoAmI{Auth: "api_key", Name: k.Name, Scope: k.Scope})
	case s.svc.Config().Server.Auth.Enabled():
		writeJSON(w, http.StatusOK, WhoAmI{Auth: "login", Scope: app.ScopeFull})
	default:
		writeJSON(w, http.StatusOK, WhoAmI{Auth: "none", Scope: app.ScopeFull})
	}
}
