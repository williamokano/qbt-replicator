package qbt_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

func newClient(t *testing.T, srv *fake.Server, user, pass string) *qbt.Client {
	t.Helper()
	c, err := qbt.New(qbt.Options{Name: "x", BaseURL: srv.URL, Username: user, Password: pass})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDialects(t *testing.T) {
	for _, v5 := range []bool{false, true} {
		name := map[bool]string{false: "v4", true: "v5"}[v5]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := fake.New(v5, "admin", "pw")
			defer src.Close()
			dst := fake.New(v5, "admin", "pw")
			defer dst.Close()
			src.Seed(qbt.Torrent{Hash: "h1", Name: "ubuntu.iso", SavePath: "/data", Size: 10}, nil, false)

			sc, dc := newClient(t, src, "admin", "pw"), newClient(t, dst, "admin", "pw")
			if err := dc.Login(ctx); err != nil {
				t.Fatal(err)
			}
			if got := dc.APIVersion().Less(qbt.Version{2, 11, 0}); got == v5 {
				t.Fatalf("version detection wrong: %v", dc.APIVersion())
			}
			data, err := sc.Export(ctx, "h1")
			if err != nil {
				t.Fatal(err)
			}
			if err := dc.AddTorrent(ctx, data, "h1", qbt.AddOptions{SavePath: "/data", Stopped: true, SkipChecking: true, Tags: []string{"r"}}); err != nil {
				t.Fatal(err)
			}
			e, _ := dst.Get("h1")
			if v5 && e.AddParams["stopped"] != "true" || !v5 && e.AddParams["paused"] != "true" {
				t.Errorf("stopped param not sent in dialect form: %v", e.AddParams)
			}
			if e.AddParams["autoTMM"] != "false" || e.AddParams["contentLayout"] != "Original" {
				t.Errorf("safety params missing: %v", e.AddParams)
			}
			tor, err := dc.Torrent(ctx, "h1")
			if err != nil || !qbt.IsStoppedState(tor.State) || !tor.HasTag("r") {
				t.Fatalf("added torrent wrong: %+v %v", tor, err)
			}
			if err := dc.Start(ctx, "h1"); err != nil {
				t.Fatal(err)
			}
			if err := dc.Stop(ctx, "h1"); err != nil {
				t.Fatal(err)
			}
			// Duplicate add reports ErrAddFailed.
			if err := dc.AddTorrent(ctx, data, "h1", qbt.AddOptions{}); !errors.Is(err, qbt.ErrAddFailed) {
				t.Errorf("duplicate add: %v", err)
			}
			priv, err := sc.IsPrivate(ctx, qbt.Torrent{Hash: "h1"})
			if err != nil || priv {
				t.Errorf("IsPrivate: %v %v", priv, err)
			}
			if err := dc.Remove(ctx, "h1"); err != nil {
				t.Fatal(err)
			}
			if dst.DeletedFiles != 0 {
				t.Fatal("remove asked to delete files")
			}
			if _, err := dc.Torrent(ctx, "h1"); !errors.Is(err, qbt.ErrNotFound) {
				t.Errorf("expected not found, got %v", err)
			}
		})
	}
}

func TestBadCredentialsBackoff(t *testing.T) {
	srv := fake.New(true, "admin", "pw")
	defer srv.Close()
	c := newClient(t, srv, "admin", "wrong")
	ctx := context.Background()
	if err := c.Login(ctx); !errors.Is(err, qbt.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	before := len(srv.Calls())
	if err := c.Login(ctx); !errors.Is(err, qbt.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	if len(srv.Calls()) != before {
		t.Error("login retried during backoff window")
	}
}

func TestReloginOnExpiredSession(t *testing.T) {
	srv := fake.New(false, "admin", "pw")
	defer srv.Close()
	srv.Seed(qbt.Torrent{Hash: "h", Name: "n", SavePath: "/d"}, nil, false)
	c := newClient(t, srv, "admin", "pw")
	ctx := context.Background()
	if _, err := c.Torrents(ctx); err != nil {
		t.Fatal(err)
	}
	srv.ExpireSession()
	ts, err := c.Torrents(ctx)
	if err != nil || len(ts) != 1 {
		t.Fatalf("relogin failed: %v %v", ts, err)
	}
	logins := 0
	for _, call := range srv.Calls() {
		if call == "auth/login" {
			logins++
		}
	}
	if logins != 2 {
		t.Errorf("expected 2 logins, got %d", logins)
	}
}

func TestExportFallbackDir(t *testing.T) {
	srv := fake.New(false, "", "")
	defer srv.Close()
	srv.NoExport = true
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "abc.torrent"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := qbt.New(qbt.Options{BaseURL: srv.URL, ExportFallbackDir: dir})
	b, err := c.Export(context.Background(), "ABC")
	if err != nil || string(b) != "data" {
		t.Fatalf("fallback: %q %v", b, err)
	}
	if _, err := c.Export(context.Background(), "zzz"); err == nil {
		t.Error("expected error for missing fallback file")
	}
}

func TestFilesRenamePriority(t *testing.T) {
	srv := fake.New(true, "", "")
	defer srv.Close()
	i0, i1 := 0, 1
	srv.Seed(qbt.Torrent{Hash: "h", Name: "show", SavePath: "/d"}, []qbt.File{
		{Index: &i0, Name: "show/a.mkv", Priority: 1}, {Index: &i1, Name: "show/b.nfo", Priority: 1},
	}, false)
	c, _ := qbt.New(qbt.Options{BaseURL: srv.URL})
	ctx := context.Background()
	if err := c.SetFilePriority(ctx, "h", []int{1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.RenameFile(ctx, "h", "show/a.mkv", "show/A.mkv"); err != nil {
		t.Fatal(err)
	}
	fs, err := c.Files(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if fs[0].Name != "show/A.mkv" || fs[1].Priority != 0 {
		t.Errorf("files: %+v", fs)
	}
	if err := c.EnsureCategory(ctx, "tv"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureCategory(ctx, "tv"); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(srv.Calls(), "torrents/createCategory"); n != 1 {
		t.Errorf("createCategory called %d times", n)
	}
}

func countCalls(calls []string, ep string) int {
	return len(slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return c != ep }))
}

func TestParseVersionAndTags(t *testing.T) {
	if v := qbt.ParseVersion("2.11.4"); v != (qbt.Version{2, 11, 4}) {
		t.Errorf("parse: %v", v)
	}
	if !qbt.ParseVersion("2.9").Less(qbt.ParseVersion("2.11.0")) {
		t.Error("less")
	}
	if !qbt.ParseVersion("garbage").IsZero() {
		t.Error("garbage")
	}
	tor := qbt.Torrent{RawTags: "a, qbtr-replica ,b"}
	if !tor.HasTag("qbtr-replica") || tor.HasTag("c") || len(tor.Tags()) != 3 {
		t.Errorf("tags: %v", tor.Tags())
	}
}
