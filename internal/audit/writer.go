package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
)

// ErrStreamTimeout is returned when the stream did not accept a record in time.
var ErrStreamTimeout = errors.New("audit: stream write timed out")

// Options configures a Writer.
type Options struct {
	WriterID string
	Store    store.Store
	Stream   io.Writer
	Timeout  time.Duration
	Now      func() time.Time // defaults to time.Now
}

// Writer appends records to one hash chain, in the store and on the stream. The chain
// head lives in the store, so processes that share a store and writer ID share the
// chain. It is safe for concurrent use; a process's records reach its stream in order.
type Writer struct {
	mu     sync.Mutex
	opt    Options
	stream *stream
}

// NewWriter checks that the store is reachable and returns a writer for
// opt.WriterID's chain.
func NewWriter(ctx context.Context, opt Options) (*Writer, error) {
	if opt.WriterID == "" || opt.Store == nil || opt.Stream == nil || opt.Timeout <= 0 {
		return nil, errors.New("audit: WriterID, Store, Stream and a positive Timeout are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if _, _, err := opt.Store.LastAudit(ctx, opt.WriterID); err != nil {
		return nil, fmt.Errorf("audit: read chain head: %w", err)
	}
	return &Writer{opt: opt, stream: newStream(opt.Stream, opt.Timeout)}, nil
}

// Write appends r to this writer's chain and returns it as written. The record reaches
// the store first; if that fails, nothing reaches the stream. If the stream then fails,
// the record is in the store and an error is still returned, because callers treat
// "not on both destinations" as a failure.
func (w *Writer) Write(ctx context.Context, r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.write(ctx, w.opt.WriterID, r)
}

func (w *Writer) write(ctx context.Context, chain string, r Record) (Record, error) {
	if r.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return Record{}, fmt.Errorf("audit: id: %w", err)
		}
		r.ID = id.String()
	}
	if r.Time.IsZero() {
		r.Time = w.opt.Now()
	}
	r.Time = r.Time.UTC().Round(0)

	sctx, cancel := context.WithTimeout(ctx, w.opt.Timeout)
	defer cancel()
	var body []byte
	_, err := w.opt.Store.AppendAuditNext(sctx, chain, func(seq uint64, prev string) (store.AuditRow, error) {
		r.Writer, r.Seq, r.Prev, r.Hash = chain, seq, prev, ""
		h, err := r.hash()
		if err != nil {
			return store.AuditRow{}, err
		}
		r.Hash = h
		if body, err = json.Marshal(r); err != nil {
			return store.AuditRow{}, fmt.Errorf("audit: encode record: %w", err)
		}
		return store.AuditRow{
			Writer: r.Writer, Seq: r.Seq, ID: r.ID, Time: r.Time,
			Phase: string(r.Phase), Hash: r.Hash, Prev: r.Prev, Body: body,
		}, nil
	})
	if err != nil {
		return Record{}, fmt.Errorf("audit: store: %w", err)
	}
	if err := w.stream.write(append(body, '\n')); err != nil {
		return r, err
	}
	return r, nil
}

// Prune removes records older than retention from every chain in the store, writing a
// checkpoint into each chain first. Chains of writers that no longer run are pruned
// too. A retention of zero keeps everything.
func (w *Writer) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := w.opt.Now().Add(-retention)
	chains, err := w.opt.Store.AuditWriters(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit: prune: %w", err)
	}
	var total int64
	for _, chain := range chains {
		n, err := w.pruneChain(ctx, chain, cutoff)
		if err != nil {
			return total, fmt.Errorf("audit: prune %q: %w", chain, err)
		}
		total += n
	}
	return total, nil
}

func (w *Writer) pruneChain(ctx context.Context, chain string, cutoff time.Time) (int64, error) {
	last, ok, err := w.opt.Store.LastAuditBefore(ctx, chain, cutoff)
	if err != nil || !ok {
		return 0, err
	}
	w.mu.Lock()
	_, err = w.write(ctx, chain, Record{
		Phase:      PhaseCheckpoint,
		Principal:  core.Principal{Subject: "opsward", Adapter: "internal"},
		Checkpoint: &Checkpoint{PrunedThroughSeq: last.Seq, PrunedThroughHash: last.Hash},
	})
	w.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("checkpoint: %w", err)
	}
	return w.opt.Store.PruneAudit(ctx, chain, last.Seq)
}

// stream writes to an io.Writer that may block, such as stdout behind a slow log
// collector. A write that does not finish within the timeout fails, and while it is
// still stuck every later write fails at once instead of piling up.
type stream struct {
	w       io.Writer
	timeout time.Duration
	busy    chan struct{}
}

func newStream(w io.Writer, timeout time.Duration) *stream {
	return &stream{w: w, timeout: timeout, busy: make(chan struct{}, 1)}
}

func (s *stream) write(p []byte) error {
	select {
	case s.busy <- struct{}{}:
	default:
		return ErrStreamTimeout
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.w.Write(p)
		<-s.busy
		done <- err
	}()
	t := time.NewTimer(s.timeout)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("audit: stream: %w", err)
		}
		return nil
	case <-t.C:
		return ErrStreamTimeout
	}
}
