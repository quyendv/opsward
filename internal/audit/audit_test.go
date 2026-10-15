package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/sqlite"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time)         { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }
func newClock(t time.Time) *clock        { return &clock{now: t} }
func openStore(t *testing.T) store.Store { return openStoreAt(t, filepath.Join(t.TempDir(), "a.db")) }

func openStoreAt(t *testing.T, path string) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newWriter(t *testing.T, s store.Store, stream *bytes.Buffer, c *clock) *Writer {
	t.Helper()
	w, err := NewWriter(context.Background(), Options{WriterID: "w1", Store: s, Stream: stream, Timeout: time.Second, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func outcome(tool string) Record {
	return Record{
		Phase:     PhaseOutcome,
		Principal: core.Principal{Subject: "alice@example.com", Adapter: "test"},
		Tool:      tool, Risk: core.RiskRead,
		Scope:   core.Scope{Environment: "staging", Namespace: "ns", Kind: "Deployment", Name: "api"},
		Args:    map[string]any{"name": "api", "replicas": 3},
		Outcome: &Outcome{Status: StatusOK},
	}
}

func TestWriteChainsRecords(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	var stream bytes.Buffer
	w := newWriter(t, s, &stream, newClock(t0))

	var written []Record
	for _, tool := range []string{"a", "b", "c"} {
		r, err := w.Write(ctx, outcome(tool))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		written = append(written, r)
	}
	if written[0].Seq != 1 || written[0].Prev != "" || written[1].Prev != written[0].Hash || written[2].Seq != 3 {
		t.Fatalf("chain fields wrong: %+v", written)
	}

	loaded, err := Load(ctx, s, "w1")
	if err != nil || len(loaded) != 3 {
		t.Fatalf("Load = %d records, %v", len(loaded), err)
	}
	if err := Verify(loaded); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	sc := bufio.NewScanner(&stream)
	var lines int
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("stream line is not a record: %v", err)
		}
		if r.Hash != written[lines].Hash {
			t.Fatalf("stream line %d hash = %s, want %s", lines, r.Hash, written[lines].Hash)
		}
		lines++
	}
	if lines != 3 {
		t.Fatalf("stream has %d lines, want 3", lines)
	}
}

func TestWriterResumesChainAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s := openStoreAt(t, path)
	var stream bytes.Buffer
	first, err := newWriter(t, s, &stream, newClock(t0)).Write(ctx, outcome("a"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2 := openStoreAt(t, path)
	second, err := newWriter(t, s2, &stream, newClock(t0)).Write(ctx, outcome("b"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != 2 || second.Prev != first.Hash {
		t.Fatalf("resumed record = seq %d prev %s; want seq 2 prev %s", second.Seq, second.Prev, first.Hash)
	}
}

// Several processes (one MCP stdio server per client session) open the same file and
// write one chain concurrently. Every write must succeed and the chain must verify.
func TestConcurrentProcessesShareOneChain(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	const processes, perProcess = 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, processes*perProcess)
	for range processes {
		var stream bytes.Buffer
		w := newWriter(t, openStoreAt(t, path), &stream, newClock(t0))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perProcess {
				if _, err := w.Write(ctx, outcome("call")); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write failed: %v", err)
	}
	records, err := Load(ctx, openStoreAt(t, path), "w1")
	if err != nil || len(records) != processes*perProcess {
		t.Fatalf("Load = %d records, %v; want %d", len(records), err, processes*perProcess)
	}
	if err := Verify(records); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A process that stayed idle while another one wrote and then pruned must chain onto
// the current head, not onto the head it last saw.
func TestWriterChainsOntoHeadAfterAnotherProcessPrunes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	var sa, sb bytes.Buffer
	cb := newClock(t0)
	a := newWriter(t, openStoreAt(t, path), &sa, newClock(t0))
	b := newWriter(t, openStoreAt(t, path), &sb, cb)
	if _, err := a.Write(ctx, outcome("a-1")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := b.Write(ctx, outcome("b")); err != nil {
			t.Fatal(err)
		}
	}
	cb.Set(t0.Add(100 * 24 * time.Hour))
	if _, err := b.Prune(ctx, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, outcome("a-2")); err != nil {
		t.Fatal(err)
	}
	records, err := Load(ctx, openStoreAt(t, path), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(records); err != nil {
		t.Fatalf("Verify: %v (records: %d)", err, len(records))
	}
}

func TestVerifyDetectsTamperingAndDeletion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	var stream bytes.Buffer
	w := newWriter(t, s, &stream, newClock(t0))
	for _, tool := range []string{"a", "b", "c"} {
		if _, err := w.Write(ctx, outcome(tool)); err != nil {
			t.Fatal(err)
		}
	}
	records, _ := Load(ctx, s, "w1")

	altered := append([]Record(nil), records...)
	altered[1].Args = map[string]any{"name": "other"}
	if err := Verify(altered); err == nil || !strings.Contains(err.Error(), "does not match its hash") {
		t.Errorf("altered record: err = %v", err)
	}

	deleted := []Record{records[0], records[2]}
	if err := Verify(deleted); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("deleted record: err = %v", err)
	}

	headless := records[1:]
	if err := Verify(headless); err == nil || !strings.Contains(err.Error(), "no checkpoint") {
		t.Errorf("records removed from the start without a checkpoint: err = %v", err)
	}
}

type failingStore struct {
	store.Store
	fail bool
}

func (f *failingStore) AppendAuditNext(ctx context.Context, writer string, build func(uint64, string) (store.AuditRow, error)) (store.AuditRow, error) {
	if f.fail {
		return store.AuditRow{}, errors.New("disk full")
	}
	return f.Store.AppendAuditNext(ctx, writer, build)
}

func TestStoreFailureDoesNotAdvanceChain(t *testing.T) {
	ctx := context.Background()
	fs := &failingStore{Store: openStore(t), fail: true}
	var stream bytes.Buffer
	w, err := NewWriter(ctx, Options{WriterID: "w1", Store: fs, Stream: &stream, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(ctx, outcome("a")); err == nil {
		t.Fatal("Write succeeded although the store failed")
	}
	if stream.Len() != 0 {
		t.Fatal("record reached the stream although the store failed")
	}
	fs.fail = false
	r, err := w.Write(ctx, outcome("b"))
	if err != nil || r.Seq != 1 {
		t.Fatalf("after recovery: seq %d, %v; want seq 1", r.Seq, err)
	}
}

type blockingWriter struct{ release chan struct{} }

func (b *blockingWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

func TestStreamTimeout(t *testing.T) {
	ctx := context.Background()
	bw := &blockingWriter{release: make(chan struct{})}
	defer close(bw.release)
	w, err := NewWriter(ctx, Options{WriterID: "w1", Store: openStore(t), Stream: bw, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := w.Write(ctx, outcome("a")); !errors.Is(err, ErrStreamTimeout) {
		t.Fatalf("err = %v, want ErrStreamTimeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Write did not honour the timeout")
	}
	start = time.Now()
	if _, err := w.Write(ctx, outcome("b")); !errors.Is(err, ErrStreamTimeout) {
		t.Fatalf("second write: err = %v, want ErrStreamTimeout", err)
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("a write behind a stuck stream should fail at once")
	}
}

type erroringWriter struct{}

func (erroringWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestStreamErrorIsReportedButRecordIsStored(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	w, err := NewWriter(ctx, Options{WriterID: "w1", Store: s, Stream: erroringWriter{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(ctx, outcome("a")); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("err = %v, want the stream error", err)
	}
	if _, ok, _ := s.LastAudit(ctx, "w1"); !ok {
		t.Fatal("record missing from the store")
	}
}

func TestPruneWritesCheckpointAndKeepsChainVerifiable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	var stream bytes.Buffer
	c := newClock(t0)
	w := newWriter(t, s, &stream, c)
	for i, tool := range []string{"old-1", "old-2", "recent"} {
		if i == 2 {
			c.Set(t0.Add(95 * 24 * time.Hour))
		}
		if _, err := w.Write(ctx, outcome(tool)); err != nil {
			t.Fatal(err)
		}
	}
	c.Set(t0.Add(100 * 24 * time.Hour))

	n, err := w.Prune(ctx, 90*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2 removed", n, err)
	}
	records, err := Load(ctx, s, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Tool != "recent" || records[1].Phase != PhaseCheckpoint {
		t.Fatalf("remaining records: %+v", records)
	}
	if err := Verify(records); err != nil {
		t.Fatalf("Verify after prune: %v", err)
	}
}

func TestPruneWithZeroRetentionKeepsEverything(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	var stream bytes.Buffer
	w := newWriter(t, s, &stream, newClock(t0))
	if _, err := w.Write(ctx, outcome("a")); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Prune(ctx, 0); err != nil || n != 0 {
		t.Fatalf("Prune(0) = %d, %v", n, err)
	}
}
