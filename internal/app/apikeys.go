package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/store"
)

// API key scopes.
const (
	ScopeRead = "read" // GET requests only
	ScopeFull = "full" // every API operation (never key management or secrets)
)

// APIKeyPrefix starts every API key, so leaked keys are easy to recognize.
const APIKeyPrefix = "qbtr_"

// ErrInvalidAPIKey is returned for unknown or malformed keys.
var ErrInvalidAPIKey = errors.New("invalid API key")

// NewAPIKey is a created key; Secret is shown once and never stored.
type NewAPIKey struct {
	store.APIKey
	Secret string `json:"secret"`
}

func hashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateAPIKey creates a key with the given scope.
func (s *Service) CreateAPIKey(ctx context.Context, name, scope string) (NewAPIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return NewAPIKey{}, errors.New("give the API key a name (e.g. home-assistant)")
	}
	if scope != ScopeRead && scope != ScopeFull {
		return NewAPIKey{}, fmt.Errorf("scope %q is invalid (read|full)", scope)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return NewAPIKey{}, err
	}
	secret := APIKeyPrefix + hex.EncodeToString(b)
	k := store.APIKey{Name: name, Prefix: secret[:len(APIKeyPrefix)+6], Scope: scope, Created: time.Now()}
	id, err := s.store.AddAPIKey(ctx, k, hashAPIKey(secret))
	if err != nil {
		return NewAPIKey{}, err
	}
	k.ID = id
	return NewAPIKey{APIKey: k, Secret: secret}, nil
}

// APIKeys lists the keys (never their secrets).
func (s *Service) APIKeys(ctx context.Context) ([]store.APIKey, error) { return s.store.APIKeys(ctx) }

// RevokeAPIKey deletes a key by id or name.
func (s *Service) RevokeAPIKey(ctx context.Context, idOrName string) (store.APIKey, error) {
	keys, err := s.store.APIKeys(ctx)
	if err != nil {
		return store.APIKey{}, err
	}
	var match []store.APIKey
	for _, k := range keys {
		if fmt.Sprint(k.ID) == idOrName || k.Name == idOrName {
			match = append(match, k)
		}
	}
	switch len(match) {
	case 0:
		return store.APIKey{}, fmt.Errorf("no API key %q", idOrName)
	case 1:
		return match[0], s.store.DeleteAPIKey(ctx, match[0].ID)
	default:
		return store.APIKey{}, fmt.Errorf("several API keys are named %q; revoke by id", idOrName)
	}
}

// HasAPIKeys reports whether any key exists (the API then requires one).
func (s *Service) HasAPIKeys(ctx context.Context) (bool, error) {
	n, err := s.store.CountAPIKeys(ctx)
	return n > 0, err
}

// AuthenticateAPIKey returns the key matching secret. Last use is recorded
// at most once a minute per key.
func (s *Service) AuthenticateAPIKey(ctx context.Context, secret string) (store.APIKey, error) {
	if !strings.HasPrefix(secret, APIKeyPrefix) {
		return store.APIKey{}, ErrInvalidAPIKey
	}
	k, err := s.store.APIKeyByHash(ctx, hashAPIKey(secret))
	if errors.Is(err, store.ErrNotFound) {
		return k, ErrInvalidAPIKey
	}
	if err != nil {
		return k, err
	}
	if now := time.Now(); now.Sub(k.LastUsed) > time.Minute {
		_ = s.store.TouchAPIKey(ctx, k.ID, now)
	}
	return k, nil
}
