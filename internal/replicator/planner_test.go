package replicator

import (
	"strings"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const tag = "qbtr-replica"

func testConfig(t *testing.T, mutate ...func(*config.Config)) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
instances:
  - {name: a, url: "http://a"}
  - {name: b, url: "http://b"}
  - {name: c, url: "http://c"}
rules:
  - name: main
    sources: [a]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.OrphanGracePeriod = 0 // tick-based tests; the period has its own test
	for _, m := range mutate {
		m(cfg)
	}
	return cfg
}

func done(hash string) qbt.Torrent {
	return qbt.Torrent{Hash: hash, Name: "name-" + hash, Progress: 1, State: qbt.StateStalledUP, SavePath: "/data",
		CompletionOn: now.Add(-time.Hour).Unix(), Category: "tv", TotalSize: 100}
}

func replica(hash string) qbt.Torrent {
	t := done(hash)
	t.RawTags = tag
	return t
}

type inst struct {
	down     bool
	torrents []qbt.Torrent
	private  map[string]bool
}

func snapshot(insts map[string]inst) *Snapshot {
	s := &Snapshot{At: now, Order: []string{"a", "b", "c"}, Instances: map[string]*InstanceState{}}
	for _, name := range s.Order {
		in := insts[name]
		st := &InstanceState{Name: name, Healthy: !in.down, Torrents: map[string]qbt.Torrent{}, Private: map[string]bool{}}
		for _, t := range in.torrents {
			st.Torrents[t.Hash] = t
			if !t.HasTag(tag) && t.Progress >= 1 {
				st.Private[t.Hash] = in.private[t.Hash]
			}
		}
		s.Instances[name] = st
	}
	return s
}

func actionsStr(p *Plan) string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, string(a.Type)+":"+a.Source+">"+a.Target+":"+a.Hash)
	}
	return strings.Join(out, " ")
}

func TestPlanner(t *testing.T) {
	downloading := done("h")
	downloading.Progress, downloading.State = 0.5, "downloading"
	moving := done("h")
	moving.State = qbt.StateMoving
	fresh := done("h")
	fresh.CompletionOn = now.Add(-30 * time.Second).Unix()
	excluded := done("h")
	excluded.RawTags = "no-replicate"
	missing := replica("h")
	missing.State = qbt.StateMissing
	incomplete := replica("h")
	incomplete.Progress, incomplete.State = 0.9, "downloading"
	moved := done("h")
	moved.SavePath = "/data/new"

	cases := []struct {
		name  string
		cfg   func(*config.Config)
		insts map[string]inst
		want  string
	}{
		{name: "completed torrent replicates to all others",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}},
			want:  "add:a>b:h add:a>c:h"},
		{name: "downloading torrent is ignored",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{downloading}}}},
		{name: "moving torrent is ignored",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{moving}}}},
		{name: "settle delay respected",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{fresh}}}},
		{name: "excluded tag",
			cfg:   func(c *config.Config) { c.Rules[0].Filter.ExcludeTags = []string{"no-replicate"} },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{excluded}}}},
		{name: "category filter",
			cfg:   func(c *config.Config) { c.Rules[0].Filter.Categories = []string{"movies"} },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}}},
		{name: "min size filter",
			cfg:   func(c *config.Config) { c.Rules[0].Filter.MinSizeBytes = 1000 },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}}},
		{name: "private excluded by default",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}, private: map[string]bool{"h": true}}}},
		{name: "private included when enabled",
			cfg:   func(c *config.Config) { c.Rules[0].Filter.IncludePrivate = true },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}, private: map[string]bool{"h": true}}},
			want:  "add:a>b:h add:a>c:h"},
		{name: "already present is skipped",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "b": {torrents: []qbt.Torrent{replica("h")}}},
			want:  "add:a>c:h"},
		{name: "unhealthy target skipped",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "c": {down: true}},
			want:  "add:a>b:h"},
		{name: "non-source instance does not replicate",
			insts: map[string]inst{"b": {torrents: []qbt.Torrent{done("h")}}}},
		{name: "mesh: any source, replicas never re-replicated",
			cfg: func(c *config.Config) { c.Rules[0].Sources = []string{"*"} },
			insts: map[string]inst{"b": {torrents: []qbt.Torrent{done("h")}}, "c": {torrents: []qbt.Torrent{replica("h")}},
				"a": {torrents: []qbt.Torrent{replica("x")}}},
			want: "add:b>a:h"}, // orphan x only counted (grace 2)
		{name: "orphan removed after grace (grace 1)",
			cfg:   func(c *config.Config) { c.Daemon.OrphanGraceTicks = 1 },
			insts: map[string]inst{"b": {torrents: []qbt.Torrent{replica("h")}}},
			want:  "remove:>b:h"},
		{name: "orphan kept when any instance is down",
			cfg:   func(c *config.Config) { c.Daemon.OrphanGraceTicks = 1 },
			insts: map[string]inst{"a": {down: true}, "b": {torrents: []qbt.Torrent{replica("h")}}}},
		{name: "orphan kept when remove_orphans disabled",
			cfg:   func(c *config.Config) { c.Daemon.OrphanGraceTicks = 1; c.Rules[0].RemoveOrphans = false },
			insts: map[string]inst{"b": {torrents: []qbt.Torrent{replica("h")}}}},
		{name: "replica in missingFiles is rechecked",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "b": {torrents: []qbt.Torrent{missing}}},
			want:  "add:a>c:h recheck:a>b:h"},
		{name: "incomplete replica is stopped",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "b": {torrents: []qbt.Torrent{incomplete}}},
			want:  "add:a>c:h stop:a>b:h"},
		{name: "relocation triggers readd",
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{moved}}, "b": {torrents: []qbt.Torrent{replica("h")}}, "c": {torrents: []qbt.Torrent{replica("h")}}},
			want:  "readd:a>b:h readd:a>c:h"},
		{name: "relocation ignored when disabled",
			cfg:   func(c *config.Config) { c.Rules[0].FollowRelocation = false },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{moved}}, "b": {torrents: []qbt.Torrent{replica("h")}}, "c": {torrents: []qbt.Torrent{replica("h")}}}},
		{name: "explicit targets",
			cfg:   func(c *config.Config) { c.Rules[0].Targets = []string{"c"} },
			insts: map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}},
			want:  "add:a>c:h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*config.Config)
			if tc.cfg != nil {
				muts = append(muts, tc.cfg)
			}
			cfg := testConfig(t, muts...)
			p := Compute(cfg, snapshot(tc.insts), nil, now)
			if got := actionsStr(p); got != tc.want {
				t.Errorf("got  %q\nwant %q\nnotes: %v", got, tc.want, p.Notes)
			}
		})
	}
}

func TestOrphanGraceAcrossTicks(t *testing.T) {
	cfg := testConfig(t)
	snap := snapshot(map[string]inst{"b": {torrents: []qbt.Torrent{replica("h")}}})
	p1 := Compute(cfg, snap, nil, now)
	if len(p1.Actions) != 0 {
		t.Fatalf("first tick should only count: %s", actionsStr(p1))
	}
	p2 := Compute(cfg, snap, p1.Memory, now)
	if actionsStr(p2) != "remove:>b:h" {
		t.Fatalf("second tick should remove: %s", actionsStr(p2))
	}
	// Source reappears: counter resets.
	back := snapshot(map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "b": {torrents: []qbt.Torrent{replica("h")}}})
	p3 := Compute(cfg, back, p1.Memory, now)
	if len(p3.Memory.Orphans) != 0 {
		t.Errorf("orphan counter not reset: %v", p3.Memory.Orphans)
	}
}

func TestRecheckOnlyOnce(t *testing.T) {
	cfg := testConfig(t)
	missing := replica("h")
	missing.State = qbt.StateMissing
	snap := snapshot(map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}, "b": {torrents: []qbt.Torrent{missing}}, "c": {torrents: []qbt.Torrent{replica("h")}}})
	p1 := Compute(cfg, snap, nil, now)
	p2 := Compute(cfg, snap, p1.Memory, now)
	if actionsStr(p1) != "recheck:a>b:h" || len(p2.Actions) != 0 || len(p2.Notes) == 0 {
		t.Errorf("p1=%s p2=%s notes=%v", actionsStr(p1), actionsStr(p2), p2.Notes)
	}
}

func TestAddActionDetails(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Instances[0].PathMappings = []config.PathMapping{{Instance: "/data", Canonical: "/shared"}}
		c.Instances[1].PathMappings = []config.PathMapping{{Instance: "/mnt/x", Canonical: "/shared"}}
		c.Rules[0].Targets = []string{"b"}
		c.Rules[0].Add.ExtraTags = []string{"seed"}
		c.Rules[0].SyncShareLimits = true
	})
	src := done("h")
	src.RatioLimit = 3
	p := Compute(cfg, snapshot(map[string]inst{"a": {torrents: []qbt.Torrent{src}}}), nil, now)
	if len(p.Actions) != 1 {
		t.Fatalf("actions: %s", actionsStr(p))
	}
	a := p.Actions[0]
	if a.SavePath != "/mnt/x" || a.Category != "tv" || strings.Join(a.Tags, ",") != tag+",seed" ||
		!a.SkipChecking || !a.Start || !a.SyncLimits || a.RatioLimit != 3 {
		t.Errorf("action: %+v", a)
	}
}

func TestPlannerExplainsSkips(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) { c.Rules[0].Sources = []string{"b"}; c.Rules[0].Targets = []string{"c"} })
	priv := done("p")
	fresh := done("f")
	fresh.CompletionOn = now.Add(-10 * time.Second).Unix()
	manual := done("m")
	rep := replica("r")
	downloading := done("d")
	downloading.Progress, downloading.State = 0.4, "downloading"
	snap := snapshot(map[string]inst{
		"b": {torrents: []qbt.Torrent{priv, fresh, manual, rep, downloading}, private: map[string]bool{"p": true}},
		"c": {torrents: []qbt.Torrent{manual}},
	})
	p := Compute(cfg, snap, nil, now)
	if len(p.Actions) != 0 {
		t.Fatalf("unexpected actions: %s", actionsStr(p))
	}
	reasons := map[string]string{}
	for _, s := range p.Skipped {
		reasons[s.Hash] = s.Reason
	}
	for hash, want := range map[string]string{
		"p": "private torrent",
		"f": "waiting for settle_delay",
		"m": "already on c as a normal torrent",
		"r": "is itself a replica",
	} {
		if !strings.Contains(reasons[hash], want) {
			t.Errorf("%s: reason %q, want it to contain %q", hash, reasons[hash], want)
		}
	}
	if _, ok := reasons["d"]; ok {
		t.Error("incomplete torrents should not be listed as skipped")
	}

	// Unreachable target is explained per target.
	snap = snapshot(map[string]inst{"b": {torrents: []qbt.Torrent{done("x")}}, "c": {down: true}})
	p = Compute(cfg, snap, nil, now)
	if len(p.Skipped) != 1 || p.Skipped[0].Target != "c" || !strings.Contains(p.Skipped[0].Reason, "unreachable") {
		t.Errorf("skipped: %+v", p.Skipped)
	}
}

func TestOrphanGracePeriod(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Daemon.OrphanGraceTicks = 1
		c.Daemon.OrphanGracePeriod = time.Minute
	})
	snap := snapshot(map[string]inst{"b": {torrents: []qbt.Torrent{replica("h")}}})
	p := Compute(cfg, snap, nil, now)
	if len(p.Actions) != 0 {
		t.Fatalf("removed before grace period: %s", actionsStr(p))
	}
	p = Compute(cfg, snap, p.Memory, now.Add(30*time.Second))
	if len(p.Actions) != 0 {
		t.Fatalf("removed after 30s: %s", actionsStr(p))
	}
	p = Compute(cfg, snap, p.Memory, now.Add(61*time.Second))
	if actionsStr(p) != "remove:>b:h" {
		t.Fatalf("not removed after period: %s", actionsStr(p))
	}
}

func TestNameAndPathFilters(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Rules[0].Filter.NameRegex = "(?i)s\\d{2}e\\d{2}"
		c.Rules[0].Filter.ExcludeNameRegex = "(?i)sample"
		c.Rules[0].Filter.SavePaths = []string{"/data/tv/"}
		c.Rules[0].Targets = []string{"b"}
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mk := func(hash, name, path string) qbt.Torrent {
		t := done(hash)
		t.Name, t.SavePath = name, path
		return t
	}
	snap := snapshot(map[string]inst{"a": {torrents: []qbt.Torrent{
		mk("ok", "Show.S01E02.1080p", "/data/tv/Show"),
		mk("movie", "Some.Movie.2024", "/data/tv"),
		mk("sample", "Show.S01E02.SAMPLE", "/data/tv"),
		mk("path", "Show.S01E03", "/data/tvx"),
	}}})
	p := Compute(cfg, snap, nil, now)
	if actionsStr(p) != "add:a>b:ok" {
		t.Fatalf("got %s skipped=%+v", actionsStr(p), p.Skipped)
	}
	if len(p.Skipped) != 3 {
		t.Errorf("expected 3 skip reasons, got %+v", p.Skipped)
	}
}

func TestInvalidNameRegex(t *testing.T) {
	_, err := config.Parse([]byte("instances: [{name: a, url: \"http://a\"}, {name: b, url: \"http://b\"}]\nrules: [{sources: [a], filter: {name_regex: \"(\"}}]\n"))
	if err == nil || !strings.Contains(err.Error(), "name_regex") {
		t.Fatalf("expected regex error, got %v", err)
	}
}

func TestFailureBackoff(t *testing.T) {
	cfg := testConfig(t)
	snap := snapshot(map[string]inst{"a": {torrents: []qbt.Torrent{done("h")}}})
	mem := NewMemory()
	fail := Result{Action: Action{Type: ActionAdd, Hash: "h", Target: "b"}, Error: "export failed"}
	mem.RecordResults([]Result{fail, {Action: Action{Type: ActionAdd, Hash: "h", Target: "c"}}}, now)

	p := Compute(cfg, snap, mem, now.Add(10*time.Second))
	if got := actionsStr(p); got != "add:a>c:h" {
		t.Fatalf("during backoff: %s", got)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Target != "b" || !strings.Contains(p.Skipped[0].Reason, "export failed") {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	if got := actionsStr(Compute(cfg, snap, mem, now.Add(retryBase))); got != "add:a>b:h add:a>c:h" {
		t.Fatalf("after backoff: %s", got)
	}

	mem.RecordResults([]Result{fail, fail}, now)
	if f := mem.Failures["b|h"]; f.Count != 3 || f.Until != now.Add(4*retryBase) {
		t.Fatalf("failure = %+v", f)
	}
	mem.RecordResults([]Result{{Action: fail.Action}}, now)
	if _, ok := mem.Failures["b|h"]; ok {
		t.Fatal("success did not clear the failure")
	}
	if retryDelay(100) != retryMax {
		t.Fatalf("retryDelay not capped: %s", retryDelay(100))
	}

	mem.RecordResults([]Result{fail}, now)
	if p := Compute(cfg, snap, mem, now.Add(25*time.Hour)); len(p.Memory.Failures) != 0 {
		t.Fatalf("stale failure kept: %+v", p.Memory.Failures)
	}
}
