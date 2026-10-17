// Package postgres implements store.Store on PostgreSQL, for more than one replica.
// docs/design/0005.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/quyendv/opsward/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is a PostgreSQL-backed store.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// Open connects with dsn and applies pending migrations. Errors never include the DSN,
// which may hold a password.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("postgres: the DSN cannot be parsed")
	}
	for k, v := range sessionDefaults {
		if _, set := cfg.ConnConfig.RuntimeParams[k]; !set {
			cfg.ConnConfig.RuntimeParams[k] = v
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// sessionDefaults bound how long a replica that dies inside a transaction holds its
// locks: without them the server waits for TCP keepalive, about two hours, while every
// other replica's audit appends wait for the lock and fail. A DSN can set its own.
var sessionDefaults = map[string]string{
	"idle_in_transaction_session_timeout": "10s",
	"tcp_keepalives_idle":                 "30",
	"tcp_keepalives_interval":             "10",
	"tcp_keepalives_count":                "3",
}

// readCommitted is set explicitly: under a database default of REPEATABLE READ the
// snapshot would be taken before the advisory lock is granted, and miss rows committed
// by whoever held it.
var readCommitted = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}

// migrate applies pending migrations in one transaction, under a transaction-scoped
// advisory lock so that replicas starting together apply each one once and no lock
// outlives a dead process.
func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("postgres: list migrations: %w", err)
	}
	sort.Strings(names)
	latest, err := version(names[len(names)-1])
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	// Up to date: nothing to create, so a runtime role needs no CREATE privilege.
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: read schema version: %w", err)
	}
	if exists {
		var current int
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("postgres: read schema version: %w", err)
		}
		if current >= latest {
			return nil
		}
	}
	err = pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('opsward:migrate:' || current_schema(), 0))`); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
			return fmt.Errorf("migrations table: %w", err)
		}
		var current int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		for _, name := range names {
			v, err := version(name)
			if err != nil {
				return err
			}
			if v <= current {
				continue
			}
			body, err := migrations.ReadFile(name)
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	return nil
}

// version is the number a migration file name starts with.
func version(name string) (int, error) {
	base := strings.TrimPrefix(name, "migrations/")
	v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
	if err != nil {
		return 0, fmt.Errorf("migration %s: name must start with a number", name)
	}
	return v, nil
}

const auditColumns = `writer, seq, id, time_ns, phase, hash, prev, body`

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAudit(ctx context.Context, q execer, r store.AuditRow) error {
	_, err := q.Exec(ctx, `INSERT INTO audit (`+auditColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.Writer, int64(r.Seq), r.ID, r.Time.UnixNano(), r.Phase, r.Hash, r.Prev, r.Body) //nolint:gosec // G115: sequence numbers stay far below 2^63
	return err
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(ctx context.Context, r store.AuditRow) error {
	if err := insertAudit(ctx, s.pool, r); err != nil {
		return fmt.Errorf("postgres: append audit: %w", err)
	}
	return nil
}

// AppendAuditNext implements store.Store. A transaction-scoped advisory lock on the
// writer serializes appenders across connections and replicas.
func (s *Store) AppendAuditNext(ctx context.Context, writer string, build func(seq uint64, prev string) (store.AuditRow, error)) (store.AuditRow, error) {
	var out store.AuditRow
	err := pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('opsward:audit:' || current_schema() || ':' || $1, 0))`, writer); err != nil {
			return err
		}
		var head int64
		var prev string
		err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit WHERE writer = $1 ORDER BY seq DESC LIMIT 1`, writer).Scan(&head, &prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		r, err := build(uint64(head)+1, prev) //nolint:gosec // G115: stored from a uint64, never negative
		if err != nil {
			return err
		}
		out = r
		return insertAudit(ctx, tx, r)
	})
	if err != nil {
		return store.AuditRow{}, fmt.Errorf("postgres: append audit: %w", err)
	}
	return out, nil
}

// LastAudit implements store.Store.
func (s *Store) LastAudit(ctx context.Context, writer string) (store.AuditRow, bool, error) {
	return s.one(ctx, `SELECT `+auditColumns+` FROM audit WHERE writer = $1 ORDER BY seq DESC LIMIT 1`, writer)
}

// LastAuditBefore implements store.Store.
func (s *Store) LastAuditBefore(ctx context.Context, writer string, t time.Time) (store.AuditRow, bool, error) {
	return s.one(ctx, `SELECT `+auditColumns+` FROM audit WHERE writer = $1 AND time_ns < $2 ORDER BY seq DESC LIMIT 1`, writer, t.UnixNano())
}

func (s *Store) one(ctx context.Context, query string, args ...any) (store.AuditRow, bool, error) {
	r, err := scanAudit(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AuditRow{}, false, nil
	}
	if err != nil {
		return store.AuditRow{}, false, fmt.Errorf("postgres: read audit: %w", err)
	}
	return r, true, nil
}

// ListAudit implements store.Store.
func (s *Store) ListAudit(ctx context.Context, writer string, afterSeq uint64, limit int) ([]store.AuditRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+auditColumns+` FROM audit WHERE writer = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
		writer, int64(afterSeq), limit) //nolint:gosec // G115: see AppendAudit
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.AuditRow, error) { return scanAudit(r) })
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit: %w", err)
	}
	return out, nil
}

// RecentAudit implements store.Store.
func (s *Store) RecentAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditRow, error) {
	query := `SELECT ` + auditColumns + ` FROM audit WHERE time_ns >= $1 AND time_ns < $2`
	args := []any{q.Since.UnixNano(), q.Until.UnixNano()}
	if b := q.Before; b != nil {
		query += ` AND (time_ns, writer, seq) < ($3, $4, $5)`
		args = append(args, b.Time.UnixNano(), b.Writer, int64(b.Seq)) //nolint:gosec // G115: see AppendAudit
	}
	query += fmt.Sprintf(` ORDER BY time_ns DESC, writer DESC, seq DESC LIMIT $%d`, len(args)+1)
	rows, err := s.pool.Query(ctx, query, append(args, q.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("postgres: recent audit: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.AuditRow, error) { return scanAudit(r) })
	if err != nil {
		return nil, fmt.Errorf("postgres: recent audit: %w", err)
	}
	return out, nil
}

// AuditWriters implements store.Store.
func (s *Store) AuditWriters(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT writer FROM audit ORDER BY writer`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit writers: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit writers: %w", err)
	}
	return out, nil
}

// PruneAudit implements store.Store.
func (s *Store) PruneAudit(ctx context.Context, writer string, throughSeq uint64) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit WHERE writer = $1 AND seq <= $2`, writer, int64(throughSeq)) //nolint:gosec // G115: see AppendAudit
	if err != nil {
		return 0, fmt.Errorf("postgres: prune audit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close implements store.Store.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

func scanAudit(row pgx.Row) (store.AuditRow, error) {
	var r store.AuditRow
	var seq, ns int64
	if err := row.Scan(&r.Writer, &seq, &r.ID, &ns, &r.Phase, &r.Hash, &r.Prev, &r.Body); err != nil {
		return store.AuditRow{}, err
	}
	r.Seq = uint64(seq) //nolint:gosec // G115: stored from a uint64, never negative
	r.Time = time.Unix(0, ns).UTC()
	return r, nil
}
