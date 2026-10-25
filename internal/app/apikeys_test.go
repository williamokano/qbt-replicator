package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/config"
)

func TestAPIKeyLifecycle(t *testing.T) {
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx := context.Background()
	if has, _ := svc.HasAPIKeys(ctx); has {
		t.Fatal("fresh database has keys")
	}
	for _, bad := range [][2]string{{"", ScopeRead}, {"x", "admin"}} {
		if _, err := svc.CreateAPIKey(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("CreateAPIKey(%q, %q) accepted", bad[0], bad[1])
		}
	}
	k, err := svc.CreateAPIKey(ctx, "ha", ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Secret, "qbtr_") || len(k.Secret) != 53 || !strings.HasPrefix(k.Secret, k.Prefix) {
		t.Fatalf("key = %+v", k)
	}
	got, err := svc.AuthenticateAPIKey(ctx, k.Secret)
	if err != nil || got.ID != k.ID || got.Scope != ScopeRead {
		t.Fatalf("authenticate = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "qbtr_nope", k.Secret[:len(k.Secret)-1], strings.TrimPrefix(k.Secret, "qbtr_")} {
		if _, err := svc.AuthenticateAPIKey(ctx, bad); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("AuthenticateAPIKey(%q) err = %v", bad, err)
		}
	}
	keys, _ := svc.APIKeys(ctx)
	if len(keys) != 1 || keys[0].LastUsed.IsZero() {
		t.Fatalf("keys = %+v", keys)
	}
	_, _ = svc.CreateAPIKey(ctx, "dup", ScopeFull)
	_, _ = svc.CreateAPIKey(ctx, "dup", ScopeFull)
	if _, err := svc.RevokeAPIKey(ctx, "dup"); err == nil {
		t.Error("ambiguous name revoked")
	}
	if _, err := svc.RevokeAPIKey(ctx, "ha"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateAPIKey(ctx, k.Secret); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatal("revoked key still works")
	}
}
