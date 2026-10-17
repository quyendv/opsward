package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/store"
)

// fakeAudit serves rows newest first, as the store does.
type fakeAudit struct {
	rows  []store.AuditRow // oldest first
	reads int
}

func (f *fakeAudit) add(t *testing.T, r audit.Record) {
	t.Helper()
	r.Writer, r.Seq = "opsward", uint64(len(f.rows)+1)
	if r.ID == "" {
		r.ID = fmt.Sprintf("r%d", r.Seq)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f.rows = append(f.rows, store.AuditRow{Writer: r.Writer, Seq: r.Seq, ID: r.ID, Time: r.Time, Phase: string(r.Phase), Body: body})
}

func (f *fakeAudit) RecentAudit(_ context.Context, q store.AuditQuery) ([]store.AuditRow, error) {
	f.reads++
	var out []store.AuditRow
	for _, r := range slices.Backward(f.rows) {
		if r.Time.Before(q.Since) || !r.Time.Before(q.Until) {
			continue
		}
		if b := q.Before; b != nil {
			if earlier := r.Time.Before(b.Time) || (r.Time.Equal(b.Time) && r.Seq < b.Seq); !earlier {
				continue
			}
		}
		if out = append(out, r); len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

var auditT0 = time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)

func outcome(at time.Duration, subject, tool, status string, scope core.Scope) audit.Record {
	return audit.Record{
		Time: auditT0.Add(at), Phase: audit.PhaseOutcome, Principal: core.Principal{Subject: subject, Adapter: "mcp"},
		Tool: tool, Scope: scope, Outcome: &audit.Outcome{Status: status},
	}
}

func searchAudit(t *testing.T, f *fakeAudit, args string) ([]engine.Item, AuditSearch) {
	t.Helper()
	tl, err := SearchAudit(f)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := tl.Prepare(json.RawMessage(args))
	if err != nil {
		t.Fatalf("Prepare(%s): %v", args, err)
	}
	items, err := prep.RunList(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	kept := make([]any, 0, len(items))
	for _, it := range items {
		kept = append(kept, it.Data)
	}
	return items, prep.Wrap(kept).(AuditSearch)
}

func ids(items []engine.Item) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Key)
	}
	return strings.Join(out, ",")
}

func TestSearchAuditIsAGlobalExactListingGrantedByName(t *testing.T) {
	tl, err := SearchAudit(&fakeAudit{})
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name != "search_audit" || tl.Risk != core.RiskRead || !tl.Global || !tl.List || !tl.Exact || !tl.ByName || !tl.NeverApproved {
		t.Fatalf("flags: %+v", tl)
	}
}

func TestSearchAuditFiltersNewestFirst(t *testing.T) {
	f := &fakeAudit{}
	api := core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}
	f.add(t, outcome(1*time.Minute, "alice@example.com", "restart_workload", audit.StatusOK, api))                                                                                    // r1
	f.add(t, outcome(2*time.Minute, "bob", "restart_workload", audit.StatusDenied, api))                                                                                              // r2
	f.add(t, outcome(3*time.Minute, "alice@example.com", "list_approvals", audit.StatusOK, core.Scope{}))                                                                             // r3
	f.add(t, audit.Record{Time: auditT0.Add(4 * time.Minute), Phase: audit.PhaseCheckpoint})                                                                                          // r4
	f.add(t, outcome(5*time.Minute, "alice@example.com", "get_workload_status", audit.StatusOK, core.Scope{Environment: "production"}))                                               // r5
	f.add(t, audit.Record{Time: auditT0.Add(6 * time.Minute), Phase: audit.PhaseIntent, Principal: core.Principal{Subject: "alice@example.com"}, Tool: "scale_workload", Scope: api}) // r6

	window := `"since":"2026-10-17T08:00:00Z","until":"2026-10-17T09:00:00Z"`
	for _, c := range []struct{ args, want string }{
		{`{` + window + `}`, "r6,r5,r3,r2,r1"},
		{`{` + window + `,"subject":"Alice@Example.com"}`, "r6,r5,r3,r1"},
		{`{` + window + `,"tool":"restart_workload"}`, "r2,r1"},
		{`{` + window + `,"status":"denied"}`, "r2"},
		{`{` + window + `,"environment":"staging"}`, "r6,r2,r1"},
		{`{` + window + `,"environment":"staging","namespace":"team-a-api"}`, "r6,r2,r1"},
		{`{"since":"2026-10-17T08:02:00Z","until":"2026-10-17T08:05:00Z"}`, "r3,r2"},
	} {
		items, _ := searchAudit(t, f, c.args)
		if got := ids(items); got != c.want {
			t.Errorf("%s: %s, want %s", c.args, got, c.want)
		}
	}
	items, _ := searchAudit(t, f, `{`+window+`,"tool":"restart_workload"}`)
	if !slicesEqualScope(items[0].Scope, api) {
		t.Fatalf("item scope = %+v: policy decides each record at its own scope", items[0].Scope)
	}
	if rec, ok := items[0].Data.(audit.Record); !ok || rec.Principal.Subject != "bob" {
		t.Fatalf("data = %#v", items[0].Data)
	}
}

func slicesEqualScope(a, b core.Scope) bool {
	return a.Environment == b.Environment && a.Namespace == b.Namespace && a.Kind == b.Kind && a.Name == b.Name
}

func TestSearchAuditPagesWithACursor(t *testing.T) {
	f := &fakeAudit{}
	for i := range 150 {
		f.add(t, outcome(time.Duration(i)*time.Second, "alice", "get_workload_status", audit.StatusOK, core.Scope{Environment: "staging"}))
	}
	window := `"since":"2026-10-17T08:00:00Z","until":"2026-10-17T09:00:00Z"`
	items, res := searchAudit(t, f, `{`+window+`}`)
	if len(items) != 100 || items[0].Key != "r150" || items[99].Key != "r51" || res.Next == "" {
		t.Fatalf("first page: %d items from %s, next %q", len(items), items[0].Key, res.Next)
	}
	items, res = searchAudit(t, f, `{`+window+`,"before":"`+res.Next+`"}`)
	if len(items) != 50 || items[0].Key != "r50" || items[49].Key != "r1" || res.Next != "" {
		t.Fatalf("second page: %d items from %s, next %q", len(items), items[0].Key, res.Next)
	}
}

// A search stops after reading 5,000 records, and says where to continue.
func TestSearchAuditStopsAfterTheScanLimit(t *testing.T) {
	f := &fakeAudit{}
	for i := range 6000 {
		f.add(t, outcome(time.Duration(i)*time.Millisecond, "bob", "get_workload_status", audit.StatusOK, core.Scope{}))
	}
	f.add(t, outcome(time.Hour-time.Second, "alice", "get_workload_status", audit.StatusOK, core.Scope{}))
	items, res := searchAudit(t, f, `{"since":"2026-10-17T08:00:00Z","until":"2026-10-17T09:00:00Z","subject":"bob"}`)
	if len(items) != 100 || res.Next == "" {
		t.Fatalf("%d items, next %q", len(items), res.Next)
	}
	items, res = searchAudit(t, f, `{"since":"2026-10-17T08:00:00Z","until":"2026-10-17T09:00:00Z","subject":"carol"}`)
	if len(items) != 0 || res.Next == "" || !strings.Contains(res.Note, "5000") {
		t.Fatalf("%d items, next %q, note %q", len(items), res.Next, res.Note)
	}
}

func TestSearchAuditRejectsBadArguments(t *testing.T) {
	tl, err := SearchAudit(&fakeAudit{})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"since":"yesterday"}`,
		`{"until":"2026-13-01T00:00:00Z"}`,
		`{"since":"2026-10-17T09:00:00Z","until":"2026-10-17T08:00:00Z"}`,
		`{"before":"not a cursor"}`,
	} {
		if _, err := tl.Prepare(json.RawMessage(args)); err == nil {
			t.Errorf("%s was accepted", args)
		}
	}
}
