package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

// issuedColumns are api_tokens columns in scan order.
const issuedColumns = `id, secret_hash, subject, groups_json, description, created_ns, expires_ns, revoked_ns, last_used_ns`

// CreateAPIToken implements store.Store.
func (s *Store) CreateAPIToken(ctx context.Context, t store.APIToken) error {
	groups, err := json.Marshal(nonNil(t.Groups))
	if err != nil {
		return fmt.Errorf("sqlite: create api token: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO api_tokens (`+issuedColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SecretHash, t.Subject, string(groups), t.Description,
		t.CreatedAt.UnixNano(), t.ExpiresAt.UnixNano(), nanos(t.RevokedAt), nanos(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create api token: %w", err)
	}
	return nil
}

// APIToken implements store.Store.
func (s *Store) APIToken(ctx context.Context, id string) (store.APIToken, bool, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+issuedColumns+` FROM api_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.APIToken{}, false, nil
	}
	if err != nil {
		return store.APIToken{}, false, fmt.Errorf("sqlite: read api token: %w", err)
	}
	return t, true, nil
}

// ListAPITokens implements store.Store.
func (s *Store) ListAPITokens(ctx context.Context) ([]store.APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+issuedColumns+` FROM api_tokens ORDER BY created_ns, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list api tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list api tokens: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list api tokens: %w", err)
	}
	return out, nil
}

// RevokeAPIToken implements store.Store.
func (s *Store) RevokeAPIToken(ctx context.Context, id string, at time.Time) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_ns = ? WHERE id = ? AND revoked_ns = 0`, at.UnixNano(), id); err != nil {
		return false, fmt.Errorf("sqlite: revoke api token: %w", err)
	}
	_, ok, err := s.APIToken(ctx, id)
	return ok, err
}

// TouchAPIToken implements store.Store.
func (s *Store) TouchAPIToken(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_ns = ? WHERE id = ?`, at.UnixNano(), id); err != nil {
		return fmt.Errorf("sqlite: touch api token: %w", err)
	}
	return nil
}

func scanToken(sc scanner) (store.APIToken, error) {
	var t store.APIToken
	var groups string
	var created, expires, revoked, used int64
	if err := sc.Scan(&t.ID, &t.SecretHash, &t.Subject, &groups, &t.Description, &created, &expires, &revoked, &used); err != nil {
		return store.APIToken{}, err
	}
	if err := json.Unmarshal([]byte(groups), &t.Groups); err != nil {
		return store.APIToken{}, fmt.Errorf("token %s groups: %w", t.ID, err)
	}
	t.CreatedAt, t.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	t.RevokedAt, t.LastUsedAt = fromNanos(revoked), fromNanos(used)
	return t, nil
}

// nanos stores the zero time as 0, the column default meaning "not set".
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
