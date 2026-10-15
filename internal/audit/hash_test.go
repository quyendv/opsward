package audit

import (
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/core"
)

// Stored records are verified by recomputing their hash from the Record struct. A
// change to Record's fields or JSON tags changes every hash and breaks verification
// of records already stored. If this test fails, that is what happened: keep old
// records verifiable (for example, new fields omitempty) instead of updating the value.
func TestRecordHashIsStable(t *testing.T) {
	r := Record{
		ID: "01a13e90-0000-7000-8000-000000000000", Writer: "opsward", Seq: 7, Prev: "sha256:00",
		Time:      time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
		Phase:     PhaseOutcome,
		Principal: core.Principal{Subject: "alice@example.com", Groups: []string{"team-a"}, Adapter: "mcp-stdio"},
		Tool:      "get_workload_status", Risk: core.RiskRead,
		Scope:    core.Scope{Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "api"},
		Args:     map[string]any{"name": "api", "replicas": float64(3)},
		Decision: &core.Decision{Effect: core.EffectAllow, Rule: "b"},
		Outcome:  &Outcome{Status: StatusOK, DurationMs: 12},
	}
	got, err := r.hash()
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:3699eb980a24d4f7a06919c67045409854447c6108d6ebbbe12c134c93b96dda"
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}
