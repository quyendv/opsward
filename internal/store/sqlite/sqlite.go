// Package sqlite implements store.Store on an embedded, pure-Go SQLite database.
// One process per database file: running several replicas on one file is not supported.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/quyendv/opsward/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One connection serializes writers, which SQLite needs anyway.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w (database %s)", err, path)
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("sqlite: migrations table: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("sqlite: migration %s: name must start with a number", name)
		}
		if v <= current {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("sqlite: read %s: %w", name, err)
		}
		if err := s.apply(ctx, v, string(body)); err != nil {
			return fmt.Errorf("sqlite: migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(ctx context.Context, r store.AuditRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (writer, seq, id, time_ns, phase, hash, prev, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Writer, int64(r.Seq), r.ID, r.Time.UnixNano(), r.Phase, r.Hash, r.Prev, r.Body) //nolint:gosec // G115: sequence numbers stay far below 2^63
	if err != nil {
		return fmt.Errorf("sqlite: append audit: %w", err)
	}
	return nil
}

const auditColumns = `writer, seq, id, time_ns, phase, hash, prev, body`

// LastAudit implements store.Store.
func (s *Store) LastAudit(ctx context.Context, writer string) (store.AuditRow, bool, error) {
	return s.one(ctx, `SELECT `+auditColumns+` FROM audit WHERE writer = ? ORDER BY seq DESC LIMIT 1`, writer)
}

// LastAuditBefore implements store.Store.
func (s *Store) LastAuditBefore(ctx context.Context, writer string, t time.Time) (store.AuditRow, bool, error) {
	return s.one(ctx, `SELECT `+auditColumns+` FROM audit WHERE writer = ? AND time_ns < ? ORDER BY seq DESC LIMIT 1`, writer, t.UnixNano())
}

func (s *Store) one(ctx context.Context, query string, args ...any) (store.AuditRow, bool, error) {
	r, err := scan(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuditRow{}, false, nil
	}
	if err != nil {
		return store.AuditRow{}, false, fmt.Errorf("sqlite: read audit: %w", err)
	}
	return r, true, nil
}

// ListAudit implements store.Store.
func (s *Store) ListAudit(ctx context.Context, writer string, afterSeq uint64, limit int) ([]store.AuditRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditColumns+` FROM audit WHERE writer = ? AND seq > ? ORDER BY seq LIMIT ?`,
		writer, int64(afterSeq), limit) //nolint:gosec // G115: see AppendAudit
	if err != nil {
		return nil, fmt.Errorf("sqlite: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.AuditRow
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list audit: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list audit: %w", err)
	}
	return out, nil
}

// PruneAudit implements store.Store.
func (s *Store) PruneAudit(ctx context.Context, writer string, throughSeq uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE writer = ? AND seq <= ?`, writer, int64(throughSeq)) //nolint:gosec // G115: see AppendAudit
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune audit: %w", err)
	}
	return res.RowsAffected()
}

// Close implements store.Store.
func (s *Store) Close() error { return s.db.Close() }

type scanner interface{ Scan(dest ...any) error }

func scan(sc scanner) (store.AuditRow, error) {
	var r store.AuditRow
	var seq, ns int64
	if err := sc.Scan(&r.Writer, &seq, &r.ID, &ns, &r.Phase, &r.Hash, &r.Prev, &r.Body); err != nil {
		return store.AuditRow{}, err
	}
	r.Seq = uint64(seq) //nolint:gosec // G115: stored from a uint64, never negative
	r.Time = time.Unix(0, ns).UTC()
	return r, nil
}
