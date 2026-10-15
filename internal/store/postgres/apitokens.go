package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/quyendv/opsward/internal/store"
)

// issuedColumns are api_tokens columns in scan order.
const issuedColumns = `id, secret_hash, subject, groups, description, created_ns, expires_ns, revoked_ns, last_used_ns`

// CreateAPIToken implements store.Store.
func (s *Store) CreateAPIToken(ctx context.Context, t store.APIToken) error {
	groups := t.Groups
	if groups == nil {
		groups = []string{}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO api_tokens (`+issuedColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.SecretHash, t.Subject, groups, t.Description,
		t.CreatedAt.UnixNano(), t.ExpiresAt.UnixNano(), nanos(t.RevokedAt), nanos(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("postgres: create api token: %w", err)
	}
	return nil
}

// APIToken implements store.Store.
func (s *Store) APIToken(ctx context.Context, id string) (store.APIToken, bool, error) {
	t, err := scanToken(s.pool.QueryRow(ctx, `SELECT `+issuedColumns+` FROM api_tokens WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.APIToken{}, false, nil
	}
	if err != nil {
		return store.APIToken{}, false, fmt.Errorf("postgres: read api token: %w", err)
	}
	return t, true, nil
}

// ListAPITokens implements store.Store.
func (s *Store) ListAPITokens(ctx context.Context) ([]store.APIToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+issuedColumns+` FROM api_tokens ORDER BY created_ns, id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api tokens: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.APIToken, error) { return scanToken(r) })
	if err != nil {
		return nil, fmt.Errorf("postgres: list api tokens: %w", err)
	}
	return out, nil
}

// RevokeAPIToken implements store.Store.
func (s *Store) RevokeAPIToken(ctx context.Context, id string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_ns = CASE WHEN revoked_ns = 0 THEN $1 ELSE revoked_ns END WHERE id = $2`, at.UnixNano(), id)
	if err != nil {
		return false, fmt.Errorf("postgres: revoke api token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// TouchAPIToken implements store.Store.
func (s *Store) TouchAPIToken(ctx context.Context, id string, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_ns = $1 WHERE id = $2`, at.UnixNano(), id); err != nil {
		return fmt.Errorf("postgres: touch api token: %w", err)
	}
	return nil
}

func scanToken(row pgx.Row) (store.APIToken, error) {
	var t store.APIToken
	var created, expires, revoked, used int64
	if err := row.Scan(&t.ID, &t.SecretHash, &t.Subject, &t.Groups, &t.Description, &created, &expires, &revoked, &used); err != nil {
		return store.APIToken{}, err
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
