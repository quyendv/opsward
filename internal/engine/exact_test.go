package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
)

// grantPolicy decides with decide, and grants the tool somewhere when granted is set.
type grantPolicy struct {
	reachPolicy
	granted bool
}

func (g grantPolicy) MayAllow(core.Request) bool { return g.granted }

var exactItems = []Item{
	{Key: "r1", Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "r1"},
	{Key: "r2", Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "r2"},
	{Key: "r3", Scope: core.Scope{Environment: "staging"}, Data: "r3"},
	{Key: "r4", Scope: core.Scope{}, Data: "r4"},
	{Key: "r5", Scope: core.Scope{Environment: "staging", Namespace: "team-b-api"}, Data: "r5"},
}

// exactTool offers its items to keep, as search_audit does, and records what it was told.
type exactTool struct {
	ran  bool
	kept []string
}

func (x *exactTool) tool() Tool {
	return Tool{
		Name: "search_records", Risk: core.RiskRead, Global: true, List: true, Exact: true,
		InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{Args: map[string]any{}, RunExact: func(_ context.Context, keep func(Item) bool) ([]Item, int, error) {
				x.ran = true
				var out []Item
				for _, it := range exactItems {
					if keep(it) {
						x.kept = append(x.kept, it.Key)
						out = append(out, it)
					}
				}
				return out, len(exactItems), nil
			}}, nil
		},
	}
}

func exactEngine(t *testing.T, x *exactTool, authz Authorizer, aud *recordingAuditor) *Engine {
	t.Helper()
	e, err := New(Options{
		Tools:        []Tool{x.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   authz, Auditor: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var teamAOnly = reachPolicy{
	decide: func(r core.Request) core.Decision {
		if r.Scope.Namespace == "team-a-api" {
			return core.Decision{Effect: core.EffectAllow, Rule: "team-a"}
		}
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	},
	// Reachability would keep the environment-wide and the unscoped items.
	reach: func(core.Request) bool { return true },
}

// An exact listing decides each item as a call at the item's scope, keeps items that
// share a scope apart by their keys, and tells the tool what it keeps.
func TestExactListDecidesEachItemAsACall(t *testing.T) {
	x := &exactTool{}
	aud := &recordingAuditor{}
	res, err := exactEngine(t, x, grantPolicy{teamAOnly, true}, aud).Invoke(context.Background(), alice, "search_records", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(res.Data)
	if string(got) != `["r1","r2"]` || len(x.kept) != 2 {
		t.Fatalf("items = %s, the tool kept %v", got, x.kept)
	}
	if l := aud.last().Outcome.Listed; l == nil || l.Read != 5 || l.Kept != 2 {
		t.Fatalf("listed = %+v", l)
	}
}

// A principal that no binding grants the tool to is denied before anything is read.
func TestExactListWithoutAGrantIsDenied(t *testing.T) {
	for name, authz := range map[string]Authorizer{"no grant": grantPolicy{teamAOnly, false}, "cannot tell": teamAOnly} {
		x := &exactTool{}
		aud := &recordingAuditor{}
		_, err := exactEngine(t, x, authz, aud).Invoke(context.Background(), alice, "search_records", json.RawMessage(`{}`))
		var denied *DeniedError
		if !errors.As(err, &denied) || x.ran || aud.last().Outcome.Status != audit.StatusDenied {
			t.Fatalf("%s: err = %v, ran = %v", name, err, x.ran)
		}
	}
}
