package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// InstanceTest is the result of testing a qBittorrent connection.
type InstanceTest struct {
	OK               bool   `json:"ok"`
	Message          string `json:"message"`
	AppVersion       string `json:"app_version,omitempty"`
	APIVersion       string `json:"api_version,omitempty"`
	Torrents         int    `json:"torrents"`
	ConnectionStatus string `json:"connection_status,omitempty"`
	LatencyMS        int64  `json:"latency_ms"`
}

// testTimeout caps a connection test so the UI answers quickly.
const testTimeout = 10 * time.Second

// TestInstance checks that an instance (saved or not) is reachable and that
// its credentials work, like the "Test" button in Sonarr/Radarr.
func (s *Service) TestInstance(ctx context.Context, in config.InstanceConfig) InstanceTest {
	in, err := in.Resolve()
	if err != nil {
		return InstanceTest{Message: err.Error()}
	}
	u, err := url.Parse(in.URL)
	if in.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return InstanceTest{Message: "Enter the WebUI URL, e.g. http://192.168.1.10:8080"}
	}
	timeout := testTimeout
	if in.Timeout > 0 && in.Timeout < timeout {
		timeout = in.Timeout
	}
	c, err := qbt.New(qbt.Options{
		Name: in.Name, BaseURL: in.URL, Username: in.Username, Password: in.Password, APIKey: in.APIKey,
		InsecureSkipVerify: in.InsecureSkipVerify, Timeout: timeout,
	})
	if err != nil {
		return InstanceTest{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := InstanceTest{}
	if err := c.Login(ctx); err != nil {
		res.Message = explainTestError(err)
		return res
	}
	ts, err := c.Torrents(ctx)
	if err != nil {
		res.Message = explainTestError(err)
		return res
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	res.OK = true
	res.AppVersion = c.AppVersion()
	res.APIVersion = c.APIVersion().String()
	res.Torrents = len(ts)
	if ti, err := c.TransferInfo(ctx); err == nil {
		res.ConnectionStatus = ti.ConnectionStatus
	}
	res.Message = fmt.Sprintf("Connected to qBittorrent %s (WebAPI %s): %d torrents, %dms", res.AppVersion, res.APIVersion, res.Torrents, res.LatencyMS)
	if res.ConnectionStatus == "firewalled" {
		res.Message += ". Note: qBittorrent reports it is firewalled (no forwarded port), so seeding will be limited."
	}
	return res
}

// explainTestError turns client errors into actionable messages.
func explainTestError(err error) string {
	var nerr net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, qbt.ErrAPIKeyRejected):
		return "API key rejected: check the key (Options → WebUI). API keys need qBittorrent 5.2 or newer."
	case errors.Is(err, qbt.ErrUnauthorized):
		return "Wrong username or password (" + err.Error() + ")"
	case errors.Is(err, qbt.ErrBanned):
		return "qBittorrent banned this IP after too many failed logins. Wait for the ban to expire (or restart qBittorrent) and check the credentials."
	case errors.As(err, &dnsErr):
		return "Host not found: " + dnsErr.Name + ". Check the URL (container names only resolve on the same Docker network)."
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()):
		return "Timed out: no answer from the WebUI. Check the URL/port and that qbt-replicator can reach it (firewall, Docker network)."
	case errors.As(err, &opErr) && strings.Contains(err.Error(), "refused"):
		return "Connection refused: nothing is listening at that address/port. Check the WebUI port."
	case strings.Contains(err.Error(), "certificate"):
		return "TLS certificate error: " + err.Error() + ". Enable 'Skip TLS verification' for self-signed certificates."
	case strings.Contains(err.Error(), "status 404"):
		return "The URL answered but is not a qBittorrent WebUI (HTTP 404). Check the URL and base path."
	default:
		return err.Error()
	}
}
