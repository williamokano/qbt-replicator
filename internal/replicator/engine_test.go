package replicator_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
)

type cluster struct {
	a, b, c *fake.Server
	eng     *replicator.Engine
}

func newCluster(t *testing.T, ruleExtra string) *cluster {
	t.Helper()
	cl := &cluster{a: fake.New(true, "u", "p"), b: fake.New(false, "u", "p"), c: fake.New(true, "u", "p")}
	t.Cleanup(func() { cl.a.Close(); cl.b.Close(); cl.c.Close() })
	doc := fmt.Sprintf(`
daemon: {orphan_grace_period: 0s}
instances:
  - {name: a, url: %q, username: u, password: p}
  - {name: b, url: %q, username: u, password: p}
  - {name: c, url: %q, username: u, password: p}
rules:
  - name: main
    sources: [a]
%s`, cl.a.URL, cl.b.URL, cl.c.URL, ruleExtra)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := replicator.NewEngine(cfg, events.New(100, slog.New(slog.NewTextHandler(io.Discard, nil))), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Executor().PollInterval = 5 * time.Millisecond
	eng.Executor().AppearTimeout = time.Second
	eng.Executor().CheckTimeout = time.Second
	eng.Executor().VerifyTimeout = 200 * time.Millisecond
	cl.eng = eng
	return cl
}

func seedShow(s *fake.Server) {
	i0, i1, i2 := 0, 1, 2
	s.Seed(qbt.Torrent{Hash: "h1", Name: "Show.S01", SavePath: "/data/tv", Category: "tv", Size: 300}, []qbt.File{
		{Index: &i0, Name: "Show.S01/e01.mkv", Size: 100, Progress: 1, Priority: 1},
		{Index: &i1, Name: "Show.S01/e02 renamed.mkv", Size: 100, Progress: 1, Priority: 1},
		{Index: &i2, Name: "Show.S01/sample.mkv", Size: 100, Progress: 0, Priority: 0},
	}, false)
	// The original .torrent had a different name for file 1 (renamed on the source).
	s.Update("h1", func(e *fake.Entry) {
		e.AddParams = map[string]string{"_orig": `[{"index":0,"name":"Show.S01/e01.mkv","size":100},{"index":1,"name":"Show.S01/e02.mkv","size":100},{"index":2,"name":"Show.S01/sample.mkv","size":100}]`}
	})
}

func failures(rep *replicator.RunReport) string {
	var out []string
	for _, r := range rep.Results {
		if r.Error != "" {
			out = append(out, r.Action.String()+": "+r.Error)
		}
	}
	return strings.Join(out, "; ")
}

func TestEndToEndReplication(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()

	rep := cl.eng.RunOnce(ctx, false)
	if rep.Failed > 0 || len(rep.Results) != 2 {
		t.Fatalf("results=%d failures=%s", len(rep.Results), failures(rep))
	}
	for _, srv := range []*fake.Server{cl.b, cl.c} {
		e, ok := srv.Get("h1")
		if !ok {
			t.Fatal("replica missing")
		}
		tor := e.Torrent
		if !tor.HasTag("qbtr-replica") || tor.SavePath != "/data/tv" || tor.Category != "tv" || tor.State != qbt.StateStalledUP {
			t.Errorf("replica wrong: %+v", tor)
		}
		if e.Files[1].Name != "Show.S01/e02 renamed.mkv" {
			t.Errorf("rename not mirrored: %+v", e.Files)
		}
		if e.Files[2].Priority != 0 || e.Files[0].Priority != 1 {
			t.Errorf("priorities not mirrored: %+v", e.Files)
		}
	}
	// Idempotent second pass.
	if rep := cl.eng.RunOnce(ctx, false); len(rep.Results) != 0 {
		t.Errorf("second pass not idempotent: %+v", rep.Plan.Actions)
	}

	// Source removes the torrent (e.g. Sonarr). Two passes later replicas go, files kept.
	cl.a.Delete("h1")
	cl.eng.RunOnce(ctx, false)
	if _, ok := cl.b.Get("h1"); !ok {
		t.Fatal("replica removed before grace period")
	}
	rep = cl.eng.RunOnce(ctx, false)
	if rep.Failed > 0 {
		t.Fatal(failures(rep))
	}
	if _, ok := cl.b.Get("h1"); ok {
		t.Error("orphan replica not removed on b")
	}
	if _, ok := cl.c.Get("h1"); ok {
		t.Error("orphan replica not removed on c")
	}
	if cl.b.DeletedFiles+cl.c.DeletedFiles != 0 {
		t.Fatal("files were deleted")
	}
}

func TestNoOrphanRemovalWhileSourceDown(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()
	cl.eng.RunOnce(ctx, false)
	cl.a.Down = true
	for range 3 {
		cl.eng.RunOnce(ctx, false)
	}
	if _, ok := cl.b.Get("h1"); !ok {
		t.Fatal("replica removed while source was unreachable")
	}
}

func TestRelocationReadd(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()
	cl.eng.RunOnce(ctx, false)
	cl.a.Update("h1", func(e *fake.Entry) { e.Torrent.SavePath = "/data/archive" })
	rep := cl.eng.RunOnce(ctx, false)
	if rep.Failed > 0 {
		t.Fatal(failures(rep))
	}
	e, _ := cl.c.Get("h1")
	if e.Torrent.SavePath != "/data/archive" {
		t.Errorf("replica not relocated: %s", e.Torrent.SavePath)
	}
}

func TestRecheckWithoutDataLeavesReplicaStopped(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n    add: {skip_checking: false}\n")
	seedShow(cl.a)
	cl.b.HasData = func(string) bool { return false }
	rep := cl.eng.RunOnce(context.Background(), false)
	if rep.Failed != 1 {
		t.Fatalf("expected exactly one failure, got %d: %s", rep.Failed, failures(rep))
	}
	e, _ := cl.b.Get("h1")
	if !qbt.IsStoppedState(e.Torrent.State) {
		t.Errorf("replica without data must stay stopped, state=%s", e.Torrent.State)
	}
	if e, _ := cl.c.Get("h1"); e.Torrent.State != qbt.StateStalledUP {
		t.Errorf("healthy target not seeding: %s", e.Torrent.State)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	rep := cl.eng.RunOnce(context.Background(), true)
	if len(rep.Results) != 2 || !rep.Results[0].DryRun {
		t.Fatalf("dry run results: %+v", rep.Results)
	}
	if len(cl.b.Hashes())+len(cl.c.Hashes()) != 0 {
		t.Error("dry run modified targets")
	}
}

func TestSettleDelay(t *testing.T) {
	cl := newCluster(t, "")
	seedShow(cl.a)
	cl.a.Update("h1", func(e *fake.Entry) { e.Torrent.CompletionOn = time.Now().Unix() })
	if rep := cl.eng.RunOnce(context.Background(), false); len(rep.Results) != 0 {
		t.Errorf("replicated within settle delay: %+v", rep.Plan.Actions)
	}
}

func TestManualReplicateAndUnreplicate(t *testing.T) {
	cl := newCluster(t, "    filter: {categories: [movies]}\n")
	seedShow(cl.a)
	ctx := context.Background()
	if _, err := cl.eng.Replicate(ctx, replicator.ReplicateRequest{Hash: "h1"}); err == nil {
		t.Fatal("expected filter rejection without force")
	}
	res, err := cl.eng.Replicate(ctx, replicator.ReplicateRequest{Hash: "h1", To: []string{"b"}, Force: true})
	if err != nil || len(res) != 1 || res[0].Error != "" {
		t.Fatalf("manual replicate: %v %+v", err, res)
	}
	if _, ok := cl.b.Get("h1"); !ok {
		t.Fatal("not replicated")
	}
	if _, err := cl.eng.Unreplicate(ctx, replicator.UnreplicateRequest{Hash: "h1", On: []string{"a"}}); !errors.Is(err, replicator.ErrNotReplica) {
		t.Fatalf("unreplicate of source must be refused, got %v", err)
	}
	res, err = cl.eng.Unreplicate(ctx, replicator.UnreplicateRequest{Hash: "h1"})
	if err != nil || len(res) != 1 {
		t.Fatalf("unreplicate: %v %+v", err, res)
	}
	if _, ok := cl.b.Get("h1"); ok {
		t.Error("replica still present")
	}
	if _, ok := cl.a.Get("h1"); !ok {
		t.Error("source removed")
	}
}

func TestPrivateSkippedByDefault(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	cl.a.Seed(qbt.Torrent{Hash: "p1", Name: "priv", SavePath: "/d"}, nil, true)
	if rep := cl.eng.RunOnce(context.Background(), false); len(rep.Results) != 0 {
		t.Errorf("private torrent replicated: %+v", rep.Plan.Actions)
	}
}

func TestSkipCheckingWithoutDataIsStoppedAfterStart(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	cl.c.HasData = func(string) bool { return false }
	rep := cl.eng.RunOnce(context.Background(), false)
	if rep.Failed != 1 {
		t.Fatalf("expected one failure, got %d: %s", rep.Failed, failures(rep))
	}
	e, _ := cl.c.Get("h1")
	if !qbt.IsStoppedState(e.Torrent.State) {
		t.Errorf("replica must be stopped, state=%s", e.Torrent.State)
	}
	if e.AddParams["dlLimit"] != "1" {
		t.Errorf("replica download limit not applied: %v", e.AddParams)
	}
	if e, _ := cl.b.Get("h1"); e.Torrent.State != qbt.StateStalledUP {
		t.Errorf("healthy target not seeding: %s", e.Torrent.State)
	}
}

func TestStatePersistsAcrossEngines(t *testing.T) {
	for _, withDB := range []bool{false, true} {
		t.Run(fmt.Sprintf("db=%v", withDB), func(t *testing.T) {
			cl := newCluster(t, "    settle_delay: 0s\n")
			seedShow(cl.a)
			ctx := context.Background()
			dir := t.TempDir()
			cfg := *cl.eng.Config()
			cfg.Path = dir + "/config.yaml"
			mk := func() *replicator.Engine {
				var st *store.Store
				if withDB {
					var err error
					if st, err = store.Open(ctx, cfg.DatabaseURL()); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = st.Close() })
				}
				eng, err := replicator.NewEngine(&cfg, events.New(10, slog.New(slog.NewTextHandler(io.Discard, nil))), nil, st)
				if err != nil {
					t.Fatal(err)
				}
				eng.Executor().PollInterval = 5 * time.Millisecond
				return eng
			}
			mk().RunOnce(ctx, false)
			cl.a.Delete("h1")
			mk().RunOnce(ctx, false) // first confirmation, persisted
			if _, ok := cl.b.Get("h1"); !ok {
				t.Fatal("removed too early")
			}
			mk().RunOnce(ctx, false) // second confirmation from a fresh process
			if _, ok := cl.b.Get("h1"); ok {
				t.Fatal("orphan grace did not survive restart")
			}
			if _, err := os.Stat(dir + "/state.json"); (err == nil) == withDB {
				t.Errorf("state.json exists = %v with db = %v", err == nil, withDB)
			}
		})
	}
}

func TestHistoryAndStateImport(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := *cl.eng.Config()
	cfg.Path = dir + "/config.yaml"
	// A legacy state file is imported once into the database.
	if err := os.WriteFile(dir+"/state.json", []byte(`{"orphans":{"x|y":1},"rechecks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, "memory")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := replicator.NewEngine(&cfg, events.New(10, slog.New(slog.NewTextHandler(io.Discard, nil))), nil, st)
	if err != nil {
		t.Fatal(err)
	}
	eng.Executor().PollInterval = 5 * time.Millisecond
	if b, err := st.Get(ctx, "planner_memory"); err != nil || !strings.Contains(string(b), `"x|y":1`) {
		t.Fatalf("state not imported: %s %v", b, err)
	}

	eng.RunOnce(ctx, true)  // dry run: not recorded
	eng.RunOnce(ctx, false) // two adds
	eng.RunOnce(ctx, false) // nothing to do: not recorded
	if _, err := eng.Unreplicate(ctx, replicator.UnreplicateRequest{Hash: "h1", On: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	acts, err := st.ListActions(ctx, store.ActionFilter{Hash: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range acts {
		got = append(got, a.Type+">"+a.Target+":"+a.Result)
	}
	if strings.Join(got, " ") != "remove>b:ok add>c:ok add>b:ok" {
		t.Fatalf("history = %v", got)
	}
}

// Manual replication without explicit targets must follow the rule's targets,
// and repeated clicks must not spill over to instances no rule targets.
func TestManualReplicateFollowsRuleTargets(t *testing.T) {
	cl := newCluster(t, "    targets: [b]\n    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()
	for range 2 {
		res, err := cl.eng.Replicate(ctx, replicator.ReplicateRequest{Hash: "h1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Action.Target != "b" {
				t.Fatalf("replicated to %s, rule only targets b", r.Action.Target)
			}
		}
	}
	if _, ok := cl.c.Get("h1"); ok {
		t.Fatal("replicated to c without a rule")
	}
	// Explicit targets are honored.
	if _, err := cl.eng.Replicate(ctx, replicator.ReplicateRequest{Hash: "h1", To: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cl.c.Get("h1"); !ok {
		t.Fatal("explicit target ignored")
	}
}

func TestManualReplicateWithoutRuleNeedsTargets(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	cl.b.Seed(qbt.Torrent{Hash: "x", Name: "x", SavePath: "/d"}, nil, false)
	_, err := cl.eng.Replicate(context.Background(), replicator.ReplicateRequest{Hash: "x", Force: true})
	if err == nil || !strings.Contains(err.Error(), "no rule replicates from b") {
		t.Fatalf("expected no-rule error, got %v", err)
	}
}

// Remove & exclude: the source is tagged first, replicas go away and the
// daemon does not re-add them until the torrent is included again.
func TestUnreplicateExcludeAndInclude(t *testing.T) {
	cl := newCluster(t, "    settle_delay: 0s\n")
	seedShow(cl.a)
	ctx := context.Background()
	cl.eng.RunOnce(ctx, false)
	if _, ok := cl.b.Get("h1"); !ok {
		t.Fatal("setup: not replicated")
	}
	res, err := cl.eng.Unreplicate(ctx, replicator.UnreplicateRequest{Hash: "h1", Exclude: true})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action.Type != replicator.ActionExclude || res[0].Action.Target != "a" {
		t.Fatalf("exclude must run first on the source: %+v", res[0].Action)
	}
	if e, _ := cl.a.Get("h1"); !e.Torrent.HasTag("qbtr-exclude") {
		t.Fatalf("source not tagged: %q", e.Torrent.RawTags)
	}
	rep := cl.eng.RunOnce(ctx, false)
	if len(rep.Results) != 0 {
		t.Fatalf("excluded torrent re-added: %+v", rep.Plan.Actions)
	}
	if _, ok := cl.b.Get("h1"); ok {
		t.Fatal("replica on b not removed")
	}
	found := false
	for _, s := range rep.Plan.Skipped {
		found = found || strings.Contains(s.Reason, "excluded")
	}
	if !found {
		t.Errorf("skip reason missing: %+v", rep.Plan.Skipped)
	}

	if _, err := cl.eng.Include(ctx, "h1", false); err != nil {
		t.Fatal(err)
	}
	if e, _ := cl.a.Get("h1"); e.Torrent.HasTag("qbtr-exclude") {
		t.Fatal("exclude tag not removed")
	}
	if rep := cl.eng.RunOnce(ctx, false); rep.Failed > 0 || len(rep.Results) != 2 {
		t.Fatalf("not replicated again after include: %+v %s", rep.Plan.Actions, failures(rep))
	}
	if cl.a.DeletedFiles+cl.b.DeletedFiles+cl.c.DeletedFiles != 0 {
		t.Fatal("files deleted")
	}
}
