package postgres

import (
	"context"
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
