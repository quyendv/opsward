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

	"modernc.org/sqlite" // also registers the "sqlite" driver

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
	q.Add("_pragma", "foreign_keys(1)")
	// The path is URI-escaped: an unescaped "?" or "#" in it would end the path early
	// and open a different file without any error.
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One connection serializes writers, which SQLite needs anyway.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := enableWAL(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: enable WAL: %w (database %s)", err, path)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w (database %s)", err, path)
	}
	return s, nil
}

// enableWAL switches the file to WAL, which persists in the file. The switch needs an
// exclusive lock that SQLite does not wait for through busy_timeout, so when several
// processes open a fresh file at once the losers retry here.
func enableWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("journal_mode is %q", mode)
			}
			return nil
		}
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// sqliteBusy is SQLITE_BUSY, the primary result code for a lock held elsewhere.
const sqliteBusy = 5

// migrate applies pending migrations. The version is read inside the same write
// transaction that applies them, so processes opening a fresh file at the same time
// do not both try to create the schema.
func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	sort.Strings(names)
	return s.immediate(ctx, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
			return fmt.Errorf("sqlite: migrations table: %w", err)
		}
		var current int
		if err := c.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("sqlite: read schema version: %w", err)
		}
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
			if _, err := c.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("sqlite: migration %s: %w", name, err)
			}
			if _, err := c.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, v); err != nil {
				return fmt.Errorf("sqlite: migration %s: %w", name, err)
			}
		}
		return nil
	})
}

// immediate runs fn in a BEGIN IMMEDIATE transaction: it takes the database write lock
// up front, waiting up to busy_timeout for other processes, so a read inside fn cannot
// be invalidated by another writer before fn's own writes.
func (s *Store) immediate(ctx context.Context, fn func(*sql.Conn) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(c); err != nil {
		_, _ = c.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		return err
	}
	if _, err := c.ExecContext(ctx, `COMMIT`); err != nil {
		_, _ = c.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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

// AppendAuditNext implements store.Store.
func (s *Store) AppendAuditNext(ctx context.Context, writer string, build func(seq uint64, prev string) (store.AuditRow, error)) (store.AuditRow, error) {
	var out store.AuditRow
	err := s.immediate(ctx, func(c *sql.Conn) error {
		var head int64
		var prev string
		err := c.QueryRowContext(ctx, `SELECT seq, hash FROM audit WHERE writer = ? ORDER BY seq DESC LIMIT 1`, writer).Scan(&head, &prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		r, err := build(uint64(head)+1, prev) //nolint:gosec // G115: stored from a uint64, never negative
		if err != nil {
			return err
		}
		_, err = c.ExecContext(ctx,
			`INSERT INTO audit (writer, seq, id, time_ns, phase, hash, prev, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Writer, int64(r.Seq), r.ID, r.Time.UnixNano(), r.Phase, r.Hash, r.Prev, r.Body) //nolint:gosec // G115: see AppendAudit
		out = r
		return err
	})
	if err != nil {
		return store.AuditRow{}, fmt.Errorf("sqlite: append audit: %w", err)
	}
	return out, nil
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

// AuditWriters implements store.Store.
func (s *Store) AuditWriters(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT writer FROM audit ORDER BY writer`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list audit writers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, fmt.Errorf("sqlite: list audit writers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list audit writers: %w", err)
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
