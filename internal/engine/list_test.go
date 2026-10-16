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

// reachPolicy decides with decide and answers Reachable with reach.
type reachPolicy struct {
	decide func(core.Request) core.Decision
	reach  func(core.Request) bool
}

func (p reachPolicy) Decide(r core.Request) core.Decision { return p.decide(r) }
func (p reachPolicy) Reachable(r core.Request) bool       { return p.reach(r) }

type listTool struct {
	items   []Item
	ran     bool
	profile string
}

func (l *listTool) tool() Tool {
	return Tool{
		Name: "list_things", Risk: core.RiskRead, List: true,
		InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"},
				Args:  map[string]any{},
				RunList: func(_ context.Context, profile string) ([]Item, error) {
					l.ran, l.profile = true, profile
					return l.items, nil
				},
			}, nil
		},
	}
}

func listEngine(t *testing.T, lt *listTool, authz Authorizer, aud *recordingAuditor) *Engine {
	t.Helper()
	e, err := New(Options{
		Tools:        []Tool{lt.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer:   authz, Auditor: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestListKeepsOnlyItemsPolicyAllows(t *testing.T) {
	lt := &listTool{items: []Item{
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Data: "api"},
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "worker"}, Data: "worker"},
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-web"}, Data: "team-a-web"},
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-db"}, Data: "team-a-db"},
	}}
	aud := &recordingAuditor{}
	authz := reachPolicy{
		decide: func(r core.Request) core.Decision {
			if r.Scope.Name == "api" {
				return core.Decision{Effect: core.EffectAllow, Rule: "api"}
			}
			return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
		},
		reach: func(r core.Request) bool { return r.Scope.Namespace != "team-a-db" },
	}
	res, err := listEngine(t, lt, authz, aud).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(res.Data)
	if string(got) != `["api","team-a-web"]` {
		t.Fatalf("items = %s; workloads by Decide, namespaces by Reachable", got)
	}
	if lt.profile != ProfileReader {
		t.Fatalf("listed with profile %q", lt.profile)
	}
	if r := aud.last(); r.Decision.Rule != core.PerItemRule || r.Outcome.Status != audit.StatusOK {
		t.Fatalf("audit = %+v %+v", r.Decision, r.Outcome)
	}
}

// An unreachable call is denied before the backend is read.
func TestListUnreachableIsDeniedWithoutReading(t *testing.T) {
	lt := &listTool{}
	aud := &recordingAuditor{}
	authz := reachPolicy{decide: allowAll, reach: func(core.Request) bool { return false }}
	_, err := listEngine(t, lt, authz, aud).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	var denied *DeniedError
	if !errors.As(err, &denied) || lt.ran || aud.last().Outcome.Status != audit.StatusDenied {
		t.Fatalf("err = %v, ran = %v", err, lt.ran)
	}
}

// An Authorizer that cannot answer reachability denies listings: fail closed.
func TestListWithoutReacherIsDenied(t *testing.T) {
	lt := &listTool{}
	_, err := listEngine(t, lt, authorizerFunc(allowAll), &recordingAuditor{}).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	var denied *DeniedError
	if !errors.As(err, &denied) || lt.ran {
		t.Fatalf("err = %v, ran = %v", err, lt.ran)
	}
}

// A cluster-wide call reads every namespace, so an environment limited to some refuses it.
func TestClusterWideCallNeedsAnUnlimitedEnvironment(t *testing.T) {
	ran := false
	tool := Tool{
		Name: "query", Risk: core.RiskRead, ClusterWide: true, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(raw json.RawMessage) (*Prepared, error) {
			var in struct{ Environment, Namespace string }
			_ = json.Unmarshal(raw, &in)
			return &Prepared{
				Scope: core.Scope{Environment: in.Environment, Namespace: in.Namespace}, Args: map[string]any{},
				Run: func(context.Context, string) (any, error) { ran = true; return "ok", nil },
			}, nil
		},
	}
	e, err := New(Options{
		Tools:        []Tool{tool},
		Environments: []Environment{{Name: "limited", Namespaces: []string{"team-a-*"}}, {Name: "all", Namespaces: []string{"*"}}},
		Authorizer:   authorizerFunc(allowAll), Auditor: &recordingAuditor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Invoke(context.Background(), alice, "query", json.RawMessage(`{"Environment":"limited"}`)); !errors.Is(err, ErrInvalidArguments) || ran {
		t.Fatalf("limited environment: err = %v, ran = %v", err, ran)
	}
	if _, err := e.Invoke(context.Background(), alice, "query", json.RawMessage(`{"Environment":"all"}`)); err != nil || !ran {
		t.Fatalf("unlimited environment: err = %v, ran = %v", err, ran)
	}
	ran = false
	if _, err := e.Invoke(context.Background(), alice, "query", json.RawMessage(`{"Environment":"limited","Namespace":"team-a-api"}`)); err != nil || !ran {
		t.Fatalf("named namespace: err = %v, ran = %v", err, ran)
	}
}

// An item allowed only through a profile other than the reader's is not shown: the
// listing read it with the reader's permissions, not that profile's.
func TestListDropsItemsAllowedThroughAnotherProfile(t *testing.T) {
	lt := &listTool{items: []Item{
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Data: "api"},
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "web"}, Data: "web"},
	}}
	authz := reachPolicy{
		decide: func(r core.Request) core.Decision {
			if r.Scope.Name == "web" {
				return core.Decision{Effect: core.EffectAllow, Rule: "team", Profile: "team-a"}
			}
			return core.Decision{Effect: core.EffectAllow, Rule: "readers", Profile: ProfileReader}
		},
		reach: func(core.Request) bool { return true },
	}
	res, err := listEngine(t, lt, authz, &recordingAuditor{}).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(res.Data); string(got) != `["api"]` {
		t.Fatalf("items = %s", got)
	}
}

// Items outside the environment are dropped by the engine too, whatever the tool did.
func TestListDropsItemsOutsideTheEnvironment(t *testing.T) {
	lt := &listTool{items: []Item{
		{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "in"},
		{Scope: core.Scope{Environment: "staging", Namespace: "kube-system"}, Data: "out"},
	}}
	authz := reachPolicy{decide: allowAll, reach: func(core.Request) bool { return true }}
	res, err := listEngine(t, lt, authz, &recordingAuditor{}).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(res.Data); string(got) != `["in"]` {
		t.Fatalf("items = %s", got)
	}
}
