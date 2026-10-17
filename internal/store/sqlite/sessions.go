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

const sessionColumns = `id_hash, subject, groups_json, credential, created_ns, expires_ns`

// CreateSession implements store.Store.
func (s *Store) CreateSession(ctx context.Context, ss store.Session) error {
	groups, err := json.Marshal(nonNil(ss.Groups))
	if err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		ss.IDHash, ss.Subject, string(groups), ss.Credential, ss.CreatedAt.UnixNano(), ss.ExpiresAt.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	return nil
}

// Session implements store.Store.
func (s *Store) Session(ctx context.Context, idHash []byte) (store.Session, bool, error) {
	var ss store.Session
	var groups string
	var created, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id_hash = ?`, idHash).
		Scan(&ss.IDHash, &ss.Subject, &groups, &ss.Credential, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, fmt.Errorf("sqlite: read session: %w", err)
	}
	if err := json.Unmarshal([]byte(groups), &ss.Groups); err != nil {
		return store.Session{}, false, fmt.Errorf("sqlite: read session groups: %w", err)
	}
	ss.CreatedAt, ss.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	return ss, true, nil
}

// DeleteSession implements store.Store.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return nil
}

// PruneSessions implements store.Store.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_ns < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune sessions: %w", err)
	}
	return res.RowsAffected()
}
