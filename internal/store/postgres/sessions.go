package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/quyendv/opsward/internal/store"
)

const sessionColumns = `id_hash, subject, groups, credential, created_ns, expires_ns`

// CreateSession implements store.Store.
func (s *Store) CreateSession(ctx context.Context, ss store.Session) error {
	groups := ss.Groups
	if groups == nil {
		groups = []string{}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES ($1, $2, $3, $4, $5, $6)`,
		ss.IDHash, ss.Subject, groups, ss.Credential, ss.CreatedAt.UnixNano(), ss.ExpiresAt.UnixNano())
	if err != nil {
		return fmt.Errorf("postgres: create session: %w", err)
	}
	return nil
}

// Session implements store.Store.
func (s *Store) Session(ctx context.Context, idHash []byte) (store.Session, bool, error) {
	var ss store.Session
	var created, expires int64
	err := s.pool.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id_hash = $1`, idHash).
		Scan(&ss.IDHash, &ss.Subject, &ss.Groups, &ss.Credential, &created, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, fmt.Errorf("postgres: read session: %w", err)
	}
	ss.CreatedAt, ss.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	return ss, true, nil
}

// DeleteSession implements store.Store.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, idHash); err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	return nil
}

// PruneSessions implements store.Store.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_ns < $1`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("postgres: prune sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
