package postgres

import (
	"context"
	"crypto/rand"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/storetest"
)

func openDSN(t *testing.T, dsn string) store.Store {
	t.Helper()
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func opener(t *testing.T) (store.Store, func() store.Store) {
	dsn := storetest.PostgresSchema(t)
	return openDSN(t, dsn), func() store.Store { return openDSN(t, dsn) }
}

func TestConformance(t *testing.T) {
	storetest.Audit(t, opener)
	storetest.APITokens(t, func(t *testing.T) store.Store {
		s, _ := opener(t)
		return s
	})
	storetest.Approvals(t, func(t *testing.T) store.Store {
		s, _ := opener(t)
		return s
	})
	storetest.Sessions(t, func(t *testing.T) store.Store {
		s, _ := opener(t)
		return s
	})
}

// Replicas starting together must apply each migration once.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	dsn := storetest.PostgresSchema(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), dsn)
			if err != nil {
				errs <- err
				return
			}
			_ = s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Open: %v", err)
	}
	s := openDSN(t, dsn).(*Store)
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(migrationNames(t)) {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
}

func migrationNames(t *testing.T) []string {
	t.Helper()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// The DSN can carry a password; an error must not echo it.
func TestOpenErrorHidesTheDSN(t *testing.T) {
	_, err := Open(context.Background(), "postgres://opsward:s3cr3t-value@127.0.0.1:1/opsward?connect_timeout=1")
	if err == nil {
		t.Fatal("Open succeeded against a closed port")
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

// A database whose default isolation is REPEATABLE READ must not make appenders reuse a
// sequence number: the snapshot would predate the wait for the lock.
func TestAppendNextUnderRepeatableReadDefault(t *testing.T) {
	dsn := storetest.PostgresSchema(t) + "&default_transaction_isolation=repeatable%20read"
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for range 4 {
		s := openDSN(t, dsn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, err := s.AppendAuditNext(context.Background(), "w1", func(seq uint64, prev string) (store.AuditRow, error) {
					return store.AuditRow{Writer: "w1", Seq: seq, ID: rand.Text(), Phase: "outcome", Hash: rand.Text(), Prev: prev, Body: []byte("{}")}, nil
				})
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendAuditNext: %v", err)
	}
}

// A replica that dies inside a transaction must not hold the audit lock until TCP
// keepalive notices, hours later: sessions get server-side timeouts unless the DSN sets
// its own.
func TestSessionTimeouts(t *testing.T) {
	ctx := context.Background()
	show := func(s store.Store, name string) string {
		var v string
		if err := s.(*Store).pool.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	dsn := storetest.PostgresSchema(t)
	s := openDSN(t, dsn)
	for name, want := range map[string]string{"idle_in_transaction_session_timeout": "10s", "tcp_keepalives_idle": "30", "tcp_keepalives_interval": "10", "tcp_keepalives_count": "3"} {
		if got := show(s, name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if got := show(openDSN(t, dsn+"&idle_in_transaction_session_timeout=5s"), "idle_in_transaction_session_timeout"); got != "5s" {
		t.Errorf("the DSN's own value was overridden: %s", got)
	}
}

// Once migrated, the store runs with a role that can use the tables but not create any.
func TestRunsWithoutCreatePrivilege(t *testing.T) {
	ctx := context.Background()
	dsn := storetest.PostgresSchema(t)
	_ = openDSN(t, dsn) // migrates as the owner
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := u.Query().Get("search_path")
	role := "r_" + strings.ToLower(rand.Text())[:12]
	owner := openDSN(t, dsn).(*Store)
	for _, q := range []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'runtime'",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + schema + " TO " + role,
	} {
		if _, err := owner.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = owner.pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	u.User = url.UserPassword(role, "runtime")
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("Open as a role without CREATE: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.AppendAudit(ctx, store.AuditRow{Writer: "w", Seq: 1, ID: "x", Phase: "outcome", Hash: "h", Body: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
}
