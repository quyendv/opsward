package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/core"
)

// An exact listing decides each item as a call at the item's scope, and keeps items
// that share a scope apart by their keys.
func TestExactListDecidesEachItemAsACall(t *testing.T) {
	items := []Item{
		{Key: "r1", Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "r1"},
		{Key: "r2", Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "r2"},
		{Key: "r3", Scope: core.Scope{Environment: "staging"}, Data: "r3"},
		{Key: "r4", Scope: core.Scope{}, Data: "r4"},
		{Key: "r5", Scope: core.Scope{Environment: "staging", Namespace: "team-b-api"}, Data: "r5"},
	}
	tool := Tool{
		Name: "search_records", Risk: core.RiskRead, Global: true, List: true, Exact: true,
		InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{Args: map[string]any{}, RunList: func(context.Context, string) ([]Item, error) { return items, nil }}, nil
		},
	}
	authz := reachPolicy{
		decide: func(r core.Request) core.Decision {
			if r.Scope.Namespace == "team-a-api" {
				return core.Decision{Effect: core.EffectAllow, Rule: "team-a"}
			}
			return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
		},
		// Reachability would keep the environment-wide and the unscoped items.
		reach: func(core.Request) bool { return true },
	}
	e, err := New(Options{
		Tools:        []Tool{tool},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   authz, Auditor: &recordingAuditor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Invoke(context.Background(), alice, "search_records", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(res.Data)
	if string(got) != `["r1","r2"]` {
		t.Fatalf("items = %s, want those Decide allows, each kept", got)
	}
}
