package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/quyendv/opsward/internal/store"
)

// Load reads every stored record of one writer, in chain order.
func Load(ctx context.Context, s store.Store, writer string) ([]Record, error) {
	var out []Record
	var after uint64
	for {
		rows, err := s.ListAudit(ctx, writer, after, 500)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return out, nil
		}
		for _, row := range rows {
			var r Record
			if err := json.Unmarshal(row.Body, &r); err != nil {
				return nil, fmt.Errorf("audit: seq %d: decode: %w", row.Seq, err)
			}
			out = append(out, r)
			after = row.Seq
		}
	}
}

// Verify checks that records form one unbroken chain. A chain that does not start at
// seq 1 must contain the checkpoint that explains where it was cut.
func Verify(records []Record) error {
	if len(records) == 0 {
		return nil
	}
	first := records[0]
	if first.Seq == 1 {
		if first.Prev != "" {
			return fmt.Errorf("audit: seq 1 has a previous hash")
		}
	} else if !hasCheckpointFor(records, first) {
		return fmt.Errorf("audit: chain starts at seq %d with no checkpoint for the records before it", first.Seq)
	}
	for _, r := range records {
		h, err := r.hash()
		if err != nil {
			return err
		}
		if h != r.Hash {
			return fmt.Errorf("audit: seq %d: content does not match its hash", r.Seq)
		}
	}
	for i := 1; i < len(records); i++ {
		p, r := records[i-1], records[i]
		if r.Seq != p.Seq+1 {
			return fmt.Errorf("audit: records missing between seq %d and %d", p.Seq, r.Seq)
		}
		if r.Prev != p.Hash {
			return fmt.Errorf("audit: seq %d: previous hash does not match seq %d", r.Seq, p.Seq)
		}
	}
	return nil
}

func hasCheckpointFor(records []Record, first Record) bool {
	for _, r := range records {
		if r.Phase == PhaseCheckpoint && r.Checkpoint != nil &&
			r.Checkpoint.PrunedThroughSeq == first.Seq-1 && r.Checkpoint.PrunedThroughHash == first.Prev {
			return true
		}
	}
	return false
}
