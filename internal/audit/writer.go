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

// Writer appends records to one hash chain, in the store and on the stream.
// It is safe for concurrent use; records are serialized.
type Writer struct {
	mu     sync.Mutex
	opt    Options
	seq    uint64
	prev   string
	stream *stream
}

// NewWriter resumes the chain for opt.WriterID from the store.
func NewWriter(ctx context.Context, opt Options) (*Writer, error) {
	if opt.WriterID == "" || opt.Store == nil || opt.Stream == nil || opt.Timeout <= 0 {
		return nil, errors.New("audit: WriterID, Store, Stream and a positive Timeout are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	w := &Writer{opt: opt, stream: newStream(opt.Stream, opt.Timeout)}
	last, ok, err := opt.Store.LastAudit(ctx, opt.WriterID)
	if err != nil {
		return nil, fmt.Errorf("audit: resume chain: %w", err)
	}
	if ok {
		w.seq, w.prev = last.Seq, last.Hash
	}
	return w, nil
}

// Write appends r to the chain and returns it as written. The record reaches the store
// first; if that fails, the chain does not advance. If the stream then fails, the
// record is in the store and an error is still returned, because callers treat
// "not on both destinations" as a failure.
func (w *Writer) Write(ctx context.Context, r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

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
	body, err := w.appendToStore(sctx, &r)
	if err != nil {
		// Several processes may share one store and writer ID, for example one MCP
		// stdio server per client session. If another one advanced the chain, chain
		// onto its last record and try once more.
		last, ok, lerr := w.opt.Store.LastAudit(sctx, w.opt.WriterID)
		if lerr != nil || !ok || last.Seq <= w.seq {
			return Record{}, err
		}
		w.seq, w.prev = last.Seq, last.Hash
		if body, err = w.appendToStore(sctx, &r); err != nil {
			return Record{}, err
		}
	}
	w.seq, w.prev = r.Seq, r.Hash

	if err := w.stream.write(append(body, '\n')); err != nil {
		return r, err
	}
	return r, nil
}

// appendToStore seals r onto the current chain head and stores it.
func (w *Writer) appendToStore(ctx context.Context, r *Record) ([]byte, error) {
	r.Writer, r.Seq, r.Prev, r.Hash = w.opt.WriterID, w.seq+1, w.prev, ""
	h, err := r.hash()
	if err != nil {
		return nil, err
	}
	r.Hash = h
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("audit: encode record: %w", err)
	}
	if err := w.opt.Store.AppendAudit(ctx, store.AuditRow{
		Writer: r.Writer, Seq: r.Seq, ID: r.ID, Time: r.Time,
		Phase: string(r.Phase), Hash: r.Hash, Prev: r.Prev, Body: body,
	}); err != nil {
		return nil, fmt.Errorf("audit: store: %w", err)
	}
	return body, nil
}

// Prune writes a checkpoint and then removes this writer's records older than
// retention. A retention of zero keeps everything.
func (w *Writer) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := w.opt.Now().Add(-retention)
	last, ok, err := w.opt.Store.LastAuditBefore(ctx, w.opt.WriterID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("audit: prune: %w", err)
	}
	if !ok {
		return 0, nil
	}
	if _, err := w.Write(ctx, Record{
		Phase:      PhaseCheckpoint,
		Principal:  core.Principal{Subject: "opsward", Adapter: "internal"},
		Checkpoint: &Checkpoint{PrunedThroughSeq: last.Seq, PrunedThroughHash: last.Hash},
	}); err != nil {
		return 0, fmt.Errorf("audit: prune: checkpoint: %w", err)
	}
	n, err := w.opt.Store.PruneAudit(ctx, w.opt.WriterID, last.Seq)
	if err != nil {
		return 0, fmt.Errorf("audit: prune: %w", err)
	}
	return n, nil
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
