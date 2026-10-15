// Package store defines where Opsward keeps its own state. Implementations live in
// subpackages: sqlite (default, one replica) and, later, postgres.
package store

import (
	"context"
	"time"
)

// AuditRow is one audit record as stored. Body is the record's JSON, hash included.
type AuditRow struct {
	Writer string
	Seq    uint64
	ID     string
	Time   time.Time
	Phase  string
	Hash   string
	Prev   string
	Body   []byte
}

// Store is the persistence interface. Every method is safe for concurrent use.
type Store interface {
	// AppendAudit stores a row. (Writer, Seq) is unique.
	AppendAudit(ctx context.Context, r AuditRow) error
	// AppendAuditNext appends the next row of writer's chain. It reads the chain head
	// and inserts under one write lock, calling build with the next sequence number and
	// the head's hash, so several processes can share one chain.
	AppendAuditNext(ctx context.Context, writer string, build func(seq uint64, prev string) (AuditRow, error)) (AuditRow, error)
	// LastAudit returns the row with the highest Seq for writer.
	LastAudit(ctx context.Context, writer string) (AuditRow, bool, error)
	// LastAuditBefore returns the highest-Seq row for writer whose Time is before t.
	LastAuditBefore(ctx context.Context, writer string, t time.Time) (AuditRow, bool, error)
	// ListAudit returns up to limit rows for writer with Seq > afterSeq, by Seq.
	ListAudit(ctx context.Context, writer string, afterSeq uint64, limit int) ([]AuditRow, error)
	// PruneAudit deletes the rows for writer with Seq <= throughSeq.
	PruneAudit(ctx context.Context, writer string, throughSeq uint64) (int64, error)
	Close() error
}
