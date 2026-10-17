package storetest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

// Opener gives each test a fresh, empty database. The returned function opens another
// handle on that same database, as a second process or replica would.
type Opener func(t *testing.T) (store.Store, func() store.Store)

func row(writer string, seq uint64, at time.Time) store.AuditRow {
	return store.AuditRow{
		Writer: writer, Seq: seq, ID: fmt.Sprintf("%s-%d", writer, seq), Time: at,
		Phase: "outcome", Hash: fmt.Sprintf("h%d", seq), Prev: fmt.Sprintf("h%d", seq-1), Body: []byte(`{"k":"v"}`),
	}
}

// Audit checks audit storage.
func Audit(t *testing.T, open Opener) {
	ctx := context.Background()

	t.Run("append and read", func(t *testing.T) {
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
		if err != nil || !ok || last.Seq != 3 || last.Hash != "h3" || last.Prev != "h2" || last.ID != "w1-3" ||
			last.Phase != "outcome" || string(last.Body) != `{"k":"v"}` || !last.Time.Equal(t0.Add(3*time.Hour)) {
			t.Fatalf("LastAudit = %+v, %v, %v", last, ok, err)
		}
		rows, err := s.ListAudit(ctx, "w1", 1, 10)
		if err != nil || len(rows) != 2 || rows[0].Seq != 2 || rows[1].Seq != 3 {
			t.Fatalf("ListAudit = %+v, %v", rows, err)
		}
		if rows, _ := s.ListAudit(ctx, "w1", 0, 2); len(rows) != 2 {
			t.Fatalf("ListAudit limit: %d rows", len(rows))
		}
		before, ok, err := s.LastAuditBefore(ctx, "w1", t0.Add(150*time.Minute))
		if err != nil || !ok || before.Seq != 2 {
			t.Fatalf("LastAuditBefore = %+v, %v, %v; want seq 2", before, ok, err)
		}
	})

	t.Run("time keeps nanoseconds", func(t *testing.T) {
		s, _ := open(t)
		at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
		if err := s.AppendAudit(ctx, row("w1", 1, at)); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.LastAudit(ctx, "w1")
		if !got.Time.Equal(at) {
			t.Fatalf("time = %v, want %v: the hash covers it", got.Time, at)
		}
	})

	t.Run("duplicate sequence", func(t *testing.T) {
		s, _ := open(t)
		if err := s.AppendAudit(ctx, row("w1", 1, t0)); err != nil {
			t.Fatal(err)
		}
		dup := row("w1", 1, t0)
		dup.ID = "other"
		if err := s.AppendAudit(ctx, dup); err == nil {
			t.Fatal("AppendAudit accepted a duplicate (writer, seq)")
		}
	})

	t.Run("prune", func(t *testing.T) {
		s, _ := open(t)
		for i := uint64(1); i <= 4; i++ {
			if err := s.AppendAudit(ctx, row("w1", i, t0)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AppendAudit(ctx, row("w2", 1, t0)); err != nil {
			t.Fatal(err)
		}
		n, err := s.PruneAudit(ctx, "w1", 2)
		if err != nil || n != 2 {
			t.Fatalf("PruneAudit = %d, %v; want 2", n, err)
		}
		rows, _ := s.ListAudit(ctx, "w1", 0, 10)
		if len(rows) != 2 || rows[0].Seq != 3 {
			t.Fatalf("after prune: %+v", rows)
		}
		if _, ok, _ := s.LastAudit(ctx, "w2"); !ok {
			t.Fatal("pruning w1 removed w2's rows")
		}
	})

	t.Run("writers", func(t *testing.T) {
		s, _ := open(t)
		for _, w := range []string{"b", "a", "b"} {
			last, _, _ := s.LastAudit(ctx, w)
			if err := s.AppendAudit(ctx, row(w, last.Seq+1, t0)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AuditWriters(ctx)
		if err != nil || strings.Join(got, ",") != "a,b" {
			t.Fatalf("AuditWriters = %v, %v; want [a b]", got, err)
		}
	})

	// Replicas and stdio processes share one chain through the store.
	t.Run("append next from several handles", func(t *testing.T) {
		_, another := open(t)
		const handles, each = 4, 25
		var wg sync.WaitGroup
		errs := make(chan error, handles*each)
		for h := range handles {
			s := another()
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range each {
					_, err := s.AppendAuditNext(ctx, "w1", func(seq uint64, prev string) (store.AuditRow, error) {
						r := row("w1", seq, t0)
						r.ID, r.Prev, r.Hash = fmt.Sprintf("h%d-%d", h, i), prev, fmt.Sprintf("hash-%d", seq)
						return r, nil
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
		rows, err := another().ListAudit(ctx, "w1", 0, handles*each+1)
		if err != nil || len(rows) != handles*each {
			t.Fatalf("ListAudit = %d rows, %v", len(rows), err)
		}
		for i, r := range rows {
			want := ""
			if i > 0 {
				want = rows[i-1].Hash
			}
			if r.Seq != uint64(i+1) || r.Prev != want {
				t.Fatalf("row %d: seq %d prev %q; want seq %d prev %q", i, r.Seq, r.Prev, i+1, want)
			}
		}
	})

	t.Run("append next does not advance on a build error", func(t *testing.T) {
		s, _ := open(t)
		_, err := s.AppendAuditNext(ctx, "w1", func(uint64, string) (store.AuditRow, error) {
			return store.AuditRow{}, fmt.Errorf("encode failed")
		})
		if err == nil {
			t.Fatal("a build error was not returned")
		}
		if _, ok, _ := s.LastAudit(ctx, "w1"); ok {
			t.Fatal("a row was stored")
		}
	})

	t.Run("recent audit across writers", func(t *testing.T) {
		s, _ := open(t)
		// w1: 1h, 2h, 3h; w2: 2h (the same time as w1-2), 4h.
		for i := uint64(1); i <= 3; i++ {
			if err := s.AppendAudit(ctx, row("w1", i, t0.Add(time.Duration(i)*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		for i, h := range []time.Duration{2, 4} {
			if err := s.AppendAudit(ctx, row("w2", uint64(i+1), t0.Add(h*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		ids := func(rows []store.AuditRow) string {
			var out []string
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return strings.Join(out, ",")
		}
		q := store.AuditQuery{Since: t0.Add(2 * time.Hour), Until: t0.Add(4 * time.Hour), Limit: 10}
		rows, err := s.RecentAudit(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		// Newest first; since is included and until is not; a tie in time goes by writer.
		if got := ids(rows); got != "w1-3,w2-1,w1-2" {
			t.Fatalf("RecentAudit = %s", got)
		}
		q.Limit = 2
		page, _ := s.RecentAudit(ctx, q)
		if got := ids(page); got != "w1-3,w2-1" {
			t.Fatalf("first page = %s", got)
		}
		last := page[len(page)-1]
		q.Before = &store.AuditPosition{Time: last.Time, Writer: last.Writer, Seq: last.Seq}
		if got, _ := s.RecentAudit(ctx, q); ids(got) != "w1-2" {
			t.Fatalf("next page = %s", ids(got))
		}
	})

	t.Run("ping", func(t *testing.T) {
		s, _ := open(t)
		if err := s.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
