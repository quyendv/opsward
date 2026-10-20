package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/store"
)

// AuditReader is what search_audit reads.
type AuditReader interface {
	RecentAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditRow, error)
}

// auditSearchLimit is how many records a search returns, auditSearchScan how many it
// reads from the store at most, and auditSearchPage how many it reads at a time.
const (
	auditSearchLimit  = 100
	auditSearchScan   = 5000
	auditSearchPage   = 500
	auditSearchWindow = 24 * time.Hour
	// auditSearchBytes bounds a page's records, JSON-encoded as the agent receives them:
	// clients refuse a result past some 25,000 tokens (docs/design/0006).
	auditSearchBytes = 32 << 10
	// auditSearchEnvelope is kept for the rest of the result: window, next and note.
	auditSearchEnvelope = 512
)

type auditInput struct {
	Since       string `json:"since,omitempty" jsonschema:"RFC 3339 time; records at or after it. Default: a day before until"`
	Until       string `json:"until,omitempty" jsonschema:"RFC 3339 time; records before it. Default: now"`
	Subject     string `json:"subject,omitempty" jsonschema:"only calls by this principal"`
	Tool        string `json:"tool,omitempty" jsonschema:"only calls to this tool"`
	Status      string `json:"status,omitempty" jsonschema:"only calls that ended so: ok, denied, invalid, error, unknown_tool or pending"`
	Environment string `json:"environment,omitempty" jsonschema:"only calls in this environment"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"only calls in this namespace"`
	Before      string `json:"before,omitempty" jsonschema:"continue a search: the next value of its result"`
}

// AuditSearch is search_audit's result.
type AuditSearch struct {
	// Since and Until are the window searched.
	Since   string `json:"since"`
	Until   string `json:"until"`
	Records []any  `json:"records"`
	// Next continues the search where it stopped, when it stopped early.
	Next string `json:"next,omitempty"`
	Note string `json:"note,omitempty"`
}

// auditNow is the clock of the default window; tests replace it.
var auditNow = time.Now

// cursor is an AuditPosition as search_audit hands it out.
// It carries the search's window, so that later pages of a search that named none keep
// the first page's.
type cursor struct {
	T     int64  `json:"t"`
	W     string `json:"w"`
	S     uint64 `json:"s"`
	Since int64  `json:"since"`
	Until int64  `json:"until"`
}

func encodeCursor(p store.AuditPosition, q store.AuditQuery) string {
	b, _ := json.Marshal(cursor{T: p.Time.UnixNano(), W: p.Writer, S: p.Seq, Since: q.Since.UnixNano(), Until: q.Until.UnixNano()})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, errors.New("before: not a value from a previous result")
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || c.W == "" {
		return cursor{}, errors.New("before: not a value from a previous result")
	}
	return c, nil
}

func parseTime(name, v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: not an RFC 3339 time: %q", name, v)
	}
	return t, nil
}

// SearchAudit is search_audit: audit records newest first, each kept only where policy
// grants the tool at the record's scope (docs/design/0015).
func SearchAudit(r AuditReader) (engine.Tool, error) {
	in, err := newInput[auditInput](nil)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "search_audit",
		Description: "Search Opsward's audit records, newest first: who called which tool, where, with what decision and " +
			"outcome, and the change a write made. Filters narrow the search; it returns the window it searched, at most " +
			"100 records (fewer when they are large), and a next value to continue when there are more.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names,
		Global: true, List: true, Exact: true, ByName: true, NeverApproved: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			defUntil, defSince := auditNow(), time.Time{}
			var before *store.AuditPosition
			if v.Before != "" {
				c, err := decodeCursor(v.Before)
				if err != nil {
					return nil, err
				}
				before = &store.AuditPosition{Time: time.Unix(0, c.T).UTC(), Writer: c.W, Seq: c.S}
				defUntil, defSince = time.Unix(0, c.Until).UTC(), time.Unix(0, c.Since).UTC()
			}
			until, err := parseTime("until", v.Until, defUntil)
			if err != nil {
				return nil, err
			}
			if defSince.IsZero() {
				defSince = until.Add(-auditSearchWindow)
			}
			since, err := parseTime("since", v.Since, defSince)
			if err != nil {
				return nil, err
			}
			if !since.Before(until) {
				return nil, errors.New("since must be before until")
			}
			q := store.AuditQuery{Since: since, Until: until, Before: before, Limit: auditSearchPage}
			var next, note string
			return &engine.Prepared{
				Args: obj,
				RunExact: func(ctx context.Context, keep func(engine.Item) bool) ([]engine.Item, int, error) {
					items, read, last, stopped, err := scanAudit(ctx, r, q, v, keep)
					if err != nil {
						return nil, read, err
					}
					if stopped != "" {
						next = encodeCursor(last, q)
						note = stopped
					}
					return items, read, nil
				},
				Wrap: func(kept []any) any {
					return AuditSearch{
						Since: q.Since.UTC().Format(time.RFC3339), Until: q.Until.UTC().Format(time.RFC3339),
						Records: kept, Next: next, Note: note,
					}
				},
			}, nil
		},
	}, nil
}

// scanAudit reads newest first until it has found more than auditSearchLimit records
// that match the filters and that policy lets the caller see, or more than fit in
// auditSearchBytes, has read auditSearchScan records, or runs out. When it stops early it
// returns the last record it read and why.
func scanAudit(ctx context.Context, r AuditReader, q store.AuditQuery, v auditInput, keep func(engine.Item) bool) ([]engine.Item, int, store.AuditPosition, string, error) {
	var items []engine.Item
	var last, shown store.AuditPosition
	subject := core.NormalizeSubject(v.Subject)
	scanned, size := 0, 0
	for {
		rows, err := r.RecentAudit(ctx, q)
		if err != nil {
			return nil, scanned, last, "", err
		}
		for _, row := range rows {
			scanned++
			last = store.AuditPosition{Time: row.Time, Writer: row.Writer, Seq: row.Seq}
			var rec audit.Record
			if err := json.Unmarshal(row.Body, &rec); err != nil {
				return nil, scanned, last, "", fmt.Errorf("audit record %s: %w", row.ID, err)
			}
			if it := (engine.Item{Key: rec.ID, Scope: rec.Scope, Data: rec}); matches(rec, v, subject) && keep(it) {
				// One more than fits tells that there are more; the next page starts after
				// the last one shown. A first record always fits, so that a search advances.
				full := len(items) > 0 && size+len(row.Body) > auditSearchBytes-auditSearchEnvelope
				if len(items) == auditSearchLimit || full {
					return items, scanned, shown, "more records match; continue with next", nil
				}
				items = append(items, it)
				size += len(row.Body) + 1
				shown = last
			}
			if scanned == auditSearchScan {
				return items, scanned, last, fmt.Sprintf("read %d records without finishing; continue with next, or narrow the time range", auditSearchScan), nil
			}
		}
		if len(rows) < q.Limit {
			return items, scanned, last, "", nil
		}
		q.Before = &last
	}
}

func matches(rec audit.Record, v auditInput, subject string) bool {
	if rec.Phase == audit.PhaseCheckpoint {
		return false
	}
	status := ""
	if rec.Outcome != nil {
		status = rec.Outcome.Status
	}
	return (subject == "" || core.NormalizeSubject(rec.Principal.Subject) == subject) &&
		(v.Tool == "" || rec.Tool == v.Tool) &&
		(v.Status == "" || status == v.Status) &&
		(v.Environment == "" || rec.Scope.Environment == v.Environment) &&
		(v.Namespace == "" || rec.Scope.Namespace == v.Namespace)
}
