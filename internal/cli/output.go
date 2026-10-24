package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/replicator"
)

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// HumanBytes formats a byte count (IEC units).
func HumanBytes(n int64) string {
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

func printActions(w io.Writer, actions []replicator.Action) error {
	if len(actions) == 0 {
		fmt.Fprintln(w, "No actions.")
		return nil
	}
	tw := newTable(w)
	fmt.Fprintln(tw, "ACTION\tSOURCE\tTARGET\tTORRENT\tHASH\tREASON")
	for _, a := range actions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Type, dash(a.Source), a.Target, a.Name, short(a.Hash), a.Reason)
	}
	return tw.Flush()
}

func printPlan(w io.Writer, format string, p *replicator.Plan) error {
	if format == "json" {
		return printJSON(w, p)
	}
	if err := printActions(w, p.Actions); err != nil {
		return err
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintln(w, "\nCompleted but not replicated:")
		tw := newTable(w)
		fmt.Fprintln(tw, "TORRENT\tHASH\tSOURCE\tTARGET\tRULE\tREASON")
		for _, s := range p.Skipped {
			target := s.Target
			if target == "" {
				target = "(all)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", trunc(s.Name, 50), short(s.Hash), s.Source, target, s.Rule, s.Reason)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintln(w, "note:", n)
	}
	return nil
}

func printResults(w io.Writer, format string, res []replicator.Result) error {
	if format == "json" {
		return printJSON(w, res)
	}
	if len(res) == 0 {
		fmt.Fprintln(w, "Nothing to do.")
		return nil
	}
	tw := newTable(w)
	fmt.Fprintln(tw, "ACTION\tSOURCE\tTARGET\tTORRENT\tRESULT")
	for _, r := range res {
		result := "ok"
		switch {
		case r.DryRun:
			result = "dry-run"
		case r.Error != "":
			result = "ERROR: " + r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Action.Type, dash(r.Action.Source), r.Action.Target, r.Action.Name, result)
	}
	return tw.Flush()
}

func printReport(w io.Writer, format string, rep *replicator.RunReport) error {
	if format == "json" {
		return printJSON(w, rep)
	}
	if err := printResults(w, format, rep.Results); err != nil {
		return err
	}
	for _, n := range rep.Plan.Notes {
		fmt.Fprintln(w, "note:", n)
	}
	fmt.Fprintf(w, "pass finished in %s: %d action(s), %d failed%s\n", rep.Duration.Round(time.Millisecond),
		len(rep.Results), rep.Failed, map[bool]string{true: " (dry run)"}[rep.DryRun])
	return nil
}

func printStatus(w io.Writer, format string, st app.Status) error {
	if format == "json" {
		return printJSON(w, st)
	}
	tw := newTable(w)
	fmt.Fprintln(tw, "INSTANCE\tHEALTH\tVERSION\tCONNECTION\tPORT\tUP\tDOWN\tTORRENTS\tSOURCES\tREPLICAS")
	for _, in := range st.Instances {
		health := "ok"
		if !in.Healthy {
			health = "DOWN: " + in.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s/s\t%s/s\t%d\t%d\t%d\n", in.Name, health, dash(in.AppVersion),
			dash(in.ConnectionStatus), portStr(in.ListenPort), HumanBytes(in.UpSpeed), HumanBytes(in.DlSpeed),
			in.Torrents, in.Sources, in.Replicas)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "total: up %s/s, down %s/s\n", HumanBytes(st.TotalUp), HumanBytes(st.TotalDown))
	return nil
}

func printTorrents(w io.Writer, format string, instances []string, rows []app.TorrentRow) error {
	if format == "json" {
		return printJSON(w, rows)
	}
	tw := newTable(w)
	fmt.Fprintf(tw, "NAME\tHASH\tSIZE\t%s\tUP\n", strings.ToUpper(strings.Join(instances, "\t")))
	for _, r := range rows {
		cells := make([]string, len(instances))
		for i, n := range instances {
			cells[i] = CellLabel(r.Cells[n])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s/s\n", trunc(r.Name, 60), short(r.Hash), HumanBytes(r.Size),
			strings.Join(cells, "\t"), HumanBytes(r.TotalUp))
	}
	return tw.Flush()
}

// CellLabel renders one matrix cell.
func CellLabel(c app.Cell) string {
	switch {
	case !c.Present:
		return "-"
	case c.Progress < 1:
		return fmt.Sprintf("%.0f%% %s", c.Progress*100, c.State)
	case c.Replica:
		return "replica/" + c.State
	default:
		return "source/" + c.State
	}
}

func remoteEvents(ctx context.Context, w io.Writer, g *globals, limit int) error {
	u := localURL(g.cfg.Server.Listen, g.cfg.Server.BasePath) + "api/v1/events?limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if a := g.cfg.Server.Auth; a.Enabled() {
		req.SetBasicAuth(a.Username, a.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is the daemon running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", u, resp.StatusCode)
	}
	var evs []events.Event
	if err := json.NewDecoder(resp.Body).Decode(&evs); err != nil {
		return err
	}
	if g.output == "json" {
		return printJSON(w, evs)
	}
	tw := newTable(w)
	fmt.Fprintln(tw, "TIME\tLEVEL\tKIND\tINSTANCE\tMESSAGE")
	for _, e := range evs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Time.Format(time.DateTime), e.Level, e.Kind, dash(e.Instance), e.Message)
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func portStr(p int) string {
	if p == 0 {
		return "-"
	}
	return strconv.Itoa(p)
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
