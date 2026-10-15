package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
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

func TestAppendAndRead(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)

	if _, ok, err := s.LastAudit(ctx, "w1"); err != nil || ok {
		t.Fatalf("LastAudit on empty store = %v, %v; want not found", ok, err)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := s.AppendAudit(ctx, row("w1", i, t0.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}
	if err := s.AppendAudit(ctx, row("w2", 1, t0)); err != nil {
		t.Fatalf("AppendAudit other writer: %v", err)
	}

	last, ok, err := s.LastAudit(ctx, "w1")
	if err != nil || !ok || last.Seq != 3 || last.Hash != "h3" || !last.Time.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("LastAudit = %+v, %v, %v", last, ok, err)
	}
	rows, err := s.ListAudit(ctx, "w1", 1, 10)
	if err != nil || len(rows) != 2 || rows[0].Seq != 2 || rows[1].Seq != 3 {
		t.Fatalf("ListAudit = %+v, %v", rows, err)
	}
	before, ok, err := s.LastAuditBefore(ctx, "w1", t0.Add(150*time.Minute))
	if err != nil || !ok || before.Seq != 2 {
		t.Fatalf("LastAuditBefore = %+v, %v, %v; want seq 2", before, ok, err)
	}
}

func TestAppendRejectsDuplicateSequence(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	if err := s.AppendAudit(ctx, row("w1", 1, t0)); err != nil {
		t.Fatal(err)
	}
	dup := row("w1", 1, t0)
	dup.ID = "other"
	if err := s.AppendAudit(ctx, dup); err == nil {
		t.Fatal("AppendAudit accepted a duplicate (writer, seq)")
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	for i := uint64(1); i <= 4; i++ {
		if err := s.AppendAudit(ctx, row("w1", i, t0)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneAudit(ctx, "w1", 2)
	if err != nil || n != 2 {
		t.Fatalf("PruneAudit = %d, %v; want 2", n, err)
	}
	rows, _ := s.ListAudit(ctx, "w1", 0, 10)
	if len(rows) != 2 || rows[0].Seq != 3 {
		t.Fatalf("after prune: %+v", rows)
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
