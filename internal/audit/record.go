// Package audit writes hash-chained audit records to the store and to a stream.
// See docs/design/0002, "Audit".
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/quyendv/opsward/internal/core"
)

// Phase says which point of a call a record describes.
type Phase string

const (
	// PhaseIntent is written before a write or destructive call runs.
	PhaseIntent Phase = "intent"
	// PhaseOutcome is written once a call has finished, been denied or failed.
	PhaseOutcome Phase = "outcome"
	// PhaseCheckpoint is written before retention removes older records.
	PhaseCheckpoint Phase = "checkpoint"
)

// Status values for Outcome.Status.
const (
	StatusOK          = "ok"
	StatusDenied      = "denied"
	StatusInvalid     = "invalid"
	StatusError       = "error"
	StatusUnknownTool = "unknown_tool"
	// StatusPending: the call needs approval and became an approval request.
	StatusPending = "pending"
)

// Record is one audit entry. Writer, Seq, Prev and Hash are set by the Writer.
// Its JSON shape is part of every stored hash; see TestRecordHashIsStable.
type Record struct {
	ID         string         `json:"id"`
	Writer     string         `json:"writer"`
	Seq        uint64         `json:"seq"`
	Prev       string         `json:"prev"`
	Hash       string         `json:"hash"`
	Time       time.Time      `json:"time"`
	Phase      Phase          `json:"phase"`
	Principal  core.Principal `json:"principal"`
	Tool       string         `json:"tool,omitempty"`
	Risk       core.Risk      `json:"risk,omitempty"`
	Scope      core.Scope     `json:"scope"`
	Args       map[string]any `json:"args,omitempty"`
	Decision   *core.Decision `json:"decision,omitempty"`
	Outcome    *Outcome       `json:"outcome,omitempty"`
	Change     *core.Change   `json:"change,omitempty"`
	Checkpoint *Checkpoint    `json:"checkpoint,omitempty"`
}

// Outcome is how a call ended.
type Outcome struct {
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// Listed is set for listings, which are authorized per item.
	Listed *Listed `json:"listed,omitempty"`
}

// Listed counts a listing's items: read from the backend, and kept by policy.
type Listed struct {
	Read int `json:"read"`
	Kept int `json:"kept"`
}

// Checkpoint records what retention is about to remove, so the chain stays verifiable
// from the cut and a prune is distinguishable from a deletion.
type Checkpoint struct {
	PrunedThroughSeq  uint64 `json:"prunedThroughSeq"`
	PrunedThroughHash string `json:"prunedThroughHash"`
}

// hash returns the record's chain hash: SHA-256 over its JSON with Hash empty.
func (r Record) hash() (string, error) {
	r.Hash = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("audit: encode record: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
