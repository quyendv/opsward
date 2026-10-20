package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
)

// reachPolicy decides with decide; reach answers which profiles reach a scope, the reader
// alone when profiles is nil.
type reachPolicy struct {
	decide   func(core.Request) core.Decision
	reach    func(core.Request) bool
	profiles func(core.Request) []string
}

func (p reachPolicy) Decide(r core.Request) core.Decision { return p.decide(r) }
func (p reachPolicy) ReachableProfiles(r core.Request) []string {
	if p.profiles != nil {
		return p.profiles(r)
	}
	if p.reach(r) {
		return []string{ProfileReader}
	}
	return nil
}

type listTool struct {
	items     []Item
	byProfile map[string][]Item
	failFor   string
	failErr   error // what failFor fails with; a plain error by default
	ran       bool
	profile   string
	profiles  []string
	caller    core.Principal
}

func (l *listTool) tool() Tool {
	return Tool{
		Name: "list_things", Risk: core.RiskRead, List: true,
		InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging"},
				Args:  map[string]any{},
				RunList: func(ctx context.Context, profile string) ([]Item, error) {
					l.ran, l.profile = true, profile
					l.caller, _ = core.PrincipalFrom(ctx)
					l.profiles = append(l.profiles, profile)
					if profile == l.failFor {
						if l.failErr != nil {
							return nil, l.failErr
						}
						return nil, errors.New("forbidden")
					}
					if l.byProfile != nil {
						return l.byProfile[profile], nil
					}
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

// A listing runs once per profile that reaches, and keeps an item only when policy allows
// it through the profile it was read with; one profile failing does not hide the others.
func TestListRunsOncePerReachableProfile(t *testing.T) {
	shared := Item{Scope: core.Scope{Environment: "staging", Namespace: "team-a-shared"}, Data: "team-a-shared"}
	own := Item{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api"}, Data: "team-a-api"}
	api := Item{Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Data: "api"}
	lt := &listTool{byProfile: map[string][]Item{
		ProfileReader: {shared, own, api}, // the reader may see more than the policy grants through it
		"team-a":      {own, api},
	}, failFor: "team-b"}
	aud := &recordingAuditor{}
	authz := reachPolicy{
		decide: func(r core.Request) core.Decision {
			return core.Decision{Effect: core.EffectAllow, Rule: "own", Profile: "team-a"}
		},
		profiles: func(r core.Request) []string {
			switch r.Scope.Namespace {
			case "":
				return []string{ProfileReader, "team-a", "team-b"}
			case "team-a-shared":
				return []string{ProfileReader}
			default:
				return []string{"team-a"}
			}
		},
	}
	res, err := listEngine(t, lt, authz, aud).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(res.Data); string(got) != `["team-a-shared","team-a-api","api"]` {
		t.Fatalf("items = %s", got)
	}
	if strings.Join(lt.profiles, ",") != "reader,team-a,team-b" {
		t.Fatalf("listed with %v", lt.profiles)
	}
	if l := aud.last().Outcome.Listed; l == nil || l.Read != 5 || l.Kept != 3 {
		t.Fatalf("audit listed = %+v", l)
	}
}

func TestListFailsWhenEveryProfileFails(t *testing.T) {
	lt := &listTool{failFor: ProfileReader}
	authz := reachPolicy{decide: allowAll, reach: func(core.Request) bool { return true }}
	if _, err := listEngine(t, lt, authz, &recordingAuditor{}).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`)); err == nil {
		t.Fatal("no error")
	}
}

func TestListCarriesTheCaller(t *testing.T) {
	lt := &listTool{}
	authz := reachPolicy{
		decide: func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow, Rule: "all"} },
		reach:  func(core.Request) bool { return true },
	}
	if _, err := listEngine(t, lt, authz, &recordingAuditor{}).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if lt.caller.Subject != alice.Subject {
		t.Fatalf("listed as %+v, want as %s", lt.caller, alice.Subject)
	}
}

func TestListRefusedEverywhereIsForbidden(t *testing.T) {
	lt := &listTool{failFor: ProfileReader, failErr: fmt.Errorf("list: %w", core.ErrForbidden)}
	aud := &recordingAuditor{}
	authz := reachPolicy{decide: allowAll, reach: func(core.Request) bool { return true }}
	if _, err := listEngine(t, lt, authz, aud).Invoke(context.Background(), alice, "list_things", json.RawMessage(`{}`)); err == nil {
		t.Fatal("no error")
	}
	if got := aud.last().Outcome.Status; got != audit.StatusForbidden {
		t.Fatalf("status = %s, want forbidden", got)
	}
}
