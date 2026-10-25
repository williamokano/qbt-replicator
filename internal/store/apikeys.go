package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// APIKey is a stored API key. Only a hash of the secret is kept.
type APIKey struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Prefix   string    `json:"prefix"` // first characters of the key, to recognize it
	Scope    string    `json:"scope"`  // read | full
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// AddAPIKey stores a key (its hash) and returns the new id.
func (s *Store) AddAPIKey(ctx context.Context, k APIKey, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (name, prefix, hash, scope, created) VALUES (?, ?, ?, ?, ?)`,
		k.Name, k.Prefix, hash, k.Scope, unixMilli(k.Created))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// APIKeys lists keys, oldest first.
func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, prefix, scope, created, last_used FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKeyByHash finds the key with this secret hash (ErrNotFound if none).
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, prefix, scope, created, last_used FROM api_keys WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// CountAPIKeys returns how many keys exist.
func (s *Store) CountAPIKeys(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n, err
}

// DeleteAPIKey revokes a key (ErrNotFound if it does not exist).
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAPIKey records when a key was last used.
func (s *Store) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, unixMilli(at), id)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanAPIKey(row scanner) (APIKey, error) {
	var k APIKey
	var created, used int64
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scope, &created, &used); err != nil {
		return k, err
	}
	k.Created, k.LastUsed = fromMilli(created), fromMilli(used)
	return k, nil
}
