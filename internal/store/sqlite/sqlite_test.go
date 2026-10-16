package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/storetest"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opsward.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func row(writer string, seq uint64, at time.Time) store.AuditRow {
	return store.AuditRow{
		Writer: writer, Seq: seq, ID: fmt.Sprintf("%s-%d", writer, seq), Time: at,
		Phase: "outcome", Hash: fmt.Sprintf("h%d", seq), Prev: fmt.Sprintf("h%d", seq-1), Body: []byte(`{}`),
	}
}

func TestReopenKeepsDataAndSkipsAppliedMigrations(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	if err := s.AppendAudit(ctx, row("w1", 1, t0)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, ok, err := s2.LastAudit(ctx, "w1"); err != nil || !ok {
		t.Fatalf("data lost on reopen: %v, %v", ok, err)
	}
}

func TestOpenErrorNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "dir", "opsward.db")
	_, err := Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name %s", err, path)
	}
}

func TestUsesWAL(t *testing.T) {
	s, _ := open(t)
	var mode string
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
}

// MCP clients start several stdio servers at once, all opening the same fresh file.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	ctx := context.Background()
	for round := range 10 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("r%d.db", round))
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := Open(ctx, path)
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
			t.Fatalf("round %d: concurrent Open failed: %v", round, err)
		}
	}
}

func TestOpenPathWithURISpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd?name#with%25chars.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
}

func opener(t *testing.T) (store.Store, func() store.Store) {
	s, path := open(t)
	return s, func() store.Store {
		other, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
		return other
	}
}

func TestConformance(t *testing.T) {
	storetest.Audit(t, opener)
	storetest.APITokens(t, func(t *testing.T) store.Store {
		s, _ := open(t)
		return s
	})
	storetest.Approvals(t, func(t *testing.T) store.Store {
		s, _ := open(t)
		return s
	})
}
