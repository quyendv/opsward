// Package store defines where Opsward keeps its own state. Implementations live in
// subpackages: sqlite (default, one replica) and postgres (several replicas).
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
	// AuditWriters returns every writer that has rows, sorted.
	AuditWriters(ctx context.Context) ([]string, error)
	// PruneAudit deletes the rows for writer with Seq <= throughSeq.
	PruneAudit(ctx context.Context, writer string, throughSeq uint64) (int64, error)

	// CreateAPIToken stores a new token. An existing ID is an error.
	CreateAPIToken(ctx context.Context, t APIToken) error
	// APIToken returns the token with id.
	APIToken(ctx context.Context, id string) (APIToken, bool, error)
	// ListAPITokens returns every token, revoked and expired ones included, by creation.
	ListAPITokens(ctx context.Context) ([]APIToken, error)
	// RevokeAPIToken marks the token revoked at at, unless it already is. It reports
	// whether the token exists.
	RevokeAPIToken(ctx context.Context, id string, at time.Time) (bool, error)
	// TouchAPIToken records that the token was used at at.
	TouchAPIToken(ctx context.Context, id string, at time.Time) error

	// CreateApproval stores a new approval request. docs/design/0007.
	CreateApproval(ctx context.Context, a Approval) error
	// Approval returns the request with id.
	Approval(ctx context.Context, id string) (Approval, bool, error)
	// ListApprovals returns the requests created at or after since, newest first.
	ListApprovals(ctx context.Context, since time.Time) ([]Approval, error)
	// DecideApproval sets a pending request's status to approved or denied. It reports
	// whether it did: a request that is not pending is left as it is.
	DecideApproval(ctx context.Context, id, status, by, reason string, at time.Time) (bool, error)
	// UseApproval marks an approved request used, in one atomic step, and reports whether
	// it did: a request runs at most once.
	UseApproval(ctx context.Context, id string, at time.Time) (bool, error)

	// Ping reports whether the store answers queries.
	Ping(ctx context.Context) error
	Close() error
}

// APIToken is an API token as stored: the hash of its secret, never the secret.
// docs/design/0005.
type APIToken struct {
	ID          string
	SecretHash  []byte // SHA-256 of the secret
	Subject     string
	Groups      []string
	Description string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   time.Time // zero: not revoked
	LastUsedAt  time.Time // zero: never used
}

// Approval request statuses.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalUsed     = "used"
)

// Approval is a call waiting for, or carrying, an approver's decision.
type Approval struct {
	ID          string
	Requester   string // the requester's subject
	Tool        string
	Environment string
	Namespace   string
	Kind        string
	Name        string
	ArgsHash    string // the call's arguments, without the approval id
	Diff        string
	// ApproverUsers and ApproverGroups are named by the binding that required approval.
	ApproverUsers  []string
	ApproverGroups []string
	Status         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	DecidedBy      string
	DecidedAt      time.Time // zero: not decided
	Reason         string
	UsedAt         time.Time // zero: not used
}
