package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
)

// memApprovals is an in-memory approval store.
type memApprovals struct {
	mu sync.Mutex
	m  map[string]store.Approval
}

func (s *memApprovals) CreateApproval(_ context.Context, a store.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]store.Approval{}
	}
	s.m[a.ID] = a
	return nil
}

func (s *memApprovals) Approval(_ context.Context, id string) (store.Approval, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.m[id]
	return a, ok, nil
}

func (s *memApprovals) UseApproval(_ context.Context, id string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.m[id]
	if !ok || a.Status != store.ApprovalApproved || !at.Before(a.ExpiresAt) {
		return false, nil
	}
	a.Status, a.UsedAt = store.ApprovalUsed, at
	s.m[id] = a
	return true, nil
}

func (s *memApprovals) CountPendingApprovals(_ context.Context, requester string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.m {
		if a.Requester == requester && a.Status == store.ApprovalPending && now.Before(a.ExpiresAt) {
			n++
		}
	}
	return n, nil
}

func (s *memApprovals) set(id string, f func(*store.Approval)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.m[id]
	f(&a)
	s.m[id] = a
}

// scaleTool previews a diff and records who ran it.
type scaleTool struct {
	diff    string
	ran     int
	ranAs   core.Principal
	profile string
}

func (s *scaleTool) tool() Tool {
	return Tool{
		Name: "scale", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(raw json.RawMessage) (*Prepared, error) {
			var args map[string]any
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"},
				Args:  args,
				Preview: func(context.Context, string) (*core.Change, error) {
					return &core.Change{Strategy: "direct", Diff: s.diff, Previous: map[string]any{"spec.replicas": 3}}, nil
				},
				Run: func(ctx context.Context, profile string) (any, error) {
					s.ran++
					s.ranAs, _ = core.PrincipalFrom(ctx)
					s.profile = profile
					return map[string]any{"ok": true}, nil
				},
			}, nil
		},
	}
}

func approvalEngine(t *testing.T, st *scaleTool, effect core.Effect, aud *recordingAuditor, approvals Approvals) *Engine {
	t.Helper()
	e, err := New(Options{
		Tools:        []Tool{st.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer: authorizerFunc(func(core.Request) core.Decision {
			return core.Decision{Effect: effect, Rule: "b", Approvers: &core.Approvers{Groups: []string{"platform"}}}
		}),
		Auditor: aud, Approvals: approvals, ApprovalTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// An allowed write is previewed first, and the change is in its intent and outcome.
func TestWriteIsPreviewedAndAudited(t *testing.T) {
	st := &scaleTool{diff: "spec.replicas: 3 -> 5"}
	aud := &recordingAuditor{}
	if _, err := approvalEngine(t, st, core.EffectAllow, aud, nil).Invoke(context.Background(), alice, "scale", json.RawMessage(`{"replicas":5}`)); err != nil {
		t.Fatal(err)
	}
	if len(aud.records) != 2 || aud.records[0].Change == nil || aud.records[0].Change.Diff != "spec.replicas: 3 -> 5" || aud.records[1].Change == nil {
		t.Fatalf("records = %+v", aud.records)
	}
	if st.ranAs.Subject != alice.Subject || st.profile != ProfileOperator {
		t.Fatalf("ran as %+v with %s", st.ranAs, st.profile)
	}
}

func TestApprovalTicketAndRetry(t *testing.T) {
	st := &scaleTool{diff: "spec.replicas: 3 -> 5"}
	aud := &recordingAuditor{}
	ap := &memApprovals{}
	e := approvalEngine(t, st, core.EffectApprove, aud, ap)
	ctx := context.Background()

	_, err := e.Invoke(ctx, alice, "scale", json.RawMessage(`{"replicas":5}`))
	var req *ApprovalRequiredError
	if !errors.As(err, &req) || req.ID == "" || req.Diff != "spec.replicas: 3 -> 5" || st.ran != 0 {
		t.Fatalf("err = %v, ran %d", err, st.ran)
	}
	if !strings.Contains(err.Error(), req.ID) || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("message %q should name the request and the approvers", err.Error())
	}
	stored, _, _ := ap.Approval(ctx, req.ID)
	if stored.Status != store.ApprovalPending || stored.Requester != alice.Subject || stored.ApproverGroups[0] != "platform" || stored.ArgsHash == "" {
		t.Fatalf("stored = %+v", stored)
	}
	if r := aud.last(); r.Outcome.Status != audit.StatusPending || r.Decision.Approval != req.ID || r.Change == nil {
		t.Fatalf("audit = %+v %+v", r.Outcome, r.Decision)
	}

	retry := json.RawMessage(`{"replicas":5,"approval":"` + req.ID + `"}`)
	if _, err := e.Invoke(ctx, alice, "scale", retry); err == nil || st.ran != 0 {
		t.Fatalf("ran before approval: %v", err)
	}
	ap.set(req.ID, func(a *store.Approval) {
		a.Status, a.DecidedBy, a.DecidedGroups = store.ApprovalApproved, "lead@example.com", []string{"platform"}
	})

	bob := core.Principal{Subject: "bob@example.com", Adapter: "test"}
	for name, tt := range map[string]struct {
		p    core.Principal
		args string
	}{
		"other principal": {bob, `{"replicas":5,"approval":"` + req.ID + `"}`},
		"other arguments": {alice, `{"replicas":6,"approval":"` + req.ID + `"}`},
		"unknown request": {alice, `{"replicas":5,"approval":"nope"}`},
	} {
		if _, err := e.Invoke(ctx, tt.p, "scale", json.RawMessage(tt.args)); err == nil || st.ran != 0 {
			t.Fatalf("%s: ran (%v)", name, err)
		}
	}
	st.diff = "spec.replicas: 4 -> 5"
	if _, err := e.Invoke(ctx, alice, "scale", retry); err == nil || !strings.Contains(err.Error(), "changed") || st.ran != 0 {
		t.Fatalf("state changed since approval: %v", err)
	}
	st.diff = "spec.replicas: 3 -> 5"
	if _, err := e.Invoke(ctx, alice, "scale", retry); err != nil || st.ran != 1 {
		t.Fatalf("approved retry: %v, ran %d", err, st.ran)
	}
	if r := aud.last(); r.Outcome.Status != audit.StatusOK || r.Decision.Approval != req.ID || r.Decision.Effect != core.EffectApprove {
		t.Fatalf("audit = %+v", r.Decision)
	}
	if _, err := e.Invoke(ctx, alice, "scale", retry); err == nil || st.ran != 1 {
		t.Fatalf("a request ran twice: %v", err)
	}
}

func TestApprovalExpires(t *testing.T) {
	st := &scaleTool{diff: "d"}
	ap := &memApprovals{}
	e := approvalEngine(t, st, core.EffectApprove, &recordingAuditor{}, ap)
	_, err := e.Invoke(context.Background(), alice, "scale", json.RawMessage(`{}`))
	var req *ApprovalRequiredError
	if !errors.As(err, &req) {
		t.Fatal(err)
	}
	ap.set(req.ID, func(a *store.Approval) { a.Status = store.ApprovalApproved; a.ExpiresAt = time.Now().Add(-time.Second) })
	if _, err := e.Invoke(context.Background(), alice, "scale", json.RawMessage(`{"approval":"`+req.ID+`"}`)); err == nil || !strings.Contains(err.Error(), "expired") || st.ran != 0 {
		t.Fatalf("expired: %v", err)
	}
}

// Without an approval store (local mode), a call that needs approval cannot run.
func TestApprovalWithoutStoreIsDenied(t *testing.T) {
	st := &scaleTool{diff: "d"}
	_, err := approvalEngine(t, st, core.EffectApprove, &recordingAuditor{}, nil).Invoke(context.Background(), alice, "scale", json.RawMessage(`{}`))
	var denied *DeniedError
	if !errors.As(err, &denied) || st.ran != 0 {
		t.Fatalf("err = %v", err)
	}
}

// The approval argument belongs to the engine: a tool whose schema does not know it, such
// as a read under an approve binding, can still be retried with it, and it is not part
// of the arguments the tool, policy and the hash see.
func TestApprovalArgumentIsTheEngines(t *testing.T) {
	var seen []map[string]any
	strict := Tool{
		Name: "strict", Risk: core.RiskRead, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(raw json.RawMessage) (*Prepared, error) {
			var args map[string]any
			_ = json.Unmarshal(raw, &args)
			if _, ok := args["approval"]; ok {
				return nil, errors.New(`unexpected additional properties ["approval"]`)
			}
			seen = append(seen, args)
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: args,
				Run: func(context.Context, string) (any, error) { return "ok", nil },
			}, nil
		},
	}
	ap := &memApprovals{}
	e, err := New(Options{
		Tools: []Tool{strict}, Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer: authorizerFunc(func(core.Request) core.Decision {
			return core.Decision{Effect: core.EffectApprove, Rule: "b", Approvers: &core.Approvers{Users: []string{"lead@example.com"}}}
		}),
		Auditor: &recordingAuditor{}, Approvals: ap,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Invoke(context.Background(), alice, "strict", json.RawMessage(`{"x":1}`))
	var req *ApprovalRequiredError
	if !errors.As(err, &req) {
		t.Fatal(err)
	}
	ap.set(req.ID, func(a *store.Approval) { a.Status = store.ApprovalApproved; a.DecidedBy = "lead@example.com" })
	if _, err := e.Invoke(context.Background(), alice, "strict", json.RawMessage(`{"x":1,"approval":"`+req.ID+`"}`)); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// A tool that grants approvals cannot itself wait for one: that would never end.
func TestNeverApprovedToolIsDeniedUnderApprove(t *testing.T) {
	decide := Tool{
		Name: "decide", Risk: core.RiskWrite, Global: true, NeverApproved: true, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{Args: map[string]any{}, Run: func(context.Context, string) (any, error) { return "ok", nil }}, nil
		},
	}
	e, err := New(Options{
		Tools:      []Tool{decide},
		Authorizer: authorizerFunc(func(core.Request) core.Decision { return core.Decision{Effect: core.EffectApprove, Rule: "all-writes"} }),
		Auditor:    &recordingAuditor{}, Approvals: &memApprovals{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Invoke(context.Background(), alice, "decide", json.RawMessage(`{}`))
	var denied *DeniedError
	if !errors.As(err, &denied) || !strings.Contains(err.Error(), "cannot require approval") {
		t.Fatalf("err = %v", err)
	}
}

// Clients see the approval argument on every tool that can wait for one, and only when
// approvals exist.
func TestToolSchemasOfferTheApprovalArgument(t *testing.T) {
	st := &scaleTool{}
	schema := func(e *Engine) *jsonschema.Schema { return e.Tools()[0].InputSchema }
	if s := schema(approvalEngine(t, st, core.EffectAllow, &recordingAuditor{}, &memApprovals{})); s.Properties[ApprovalArg] == nil {
		t.Fatalf("properties = %v", s.Properties)
	}
	if s := schema(approvalEngine(t, st, core.EffectAllow, &recordingAuditor{}, nil)); s.Properties[ApprovalArg] != nil {
		t.Fatal("offered without approvals")
	}
	if st.tool().InputSchema.Properties != nil {
		t.Fatal("the tool's own schema was modified")
	}
}

// An approval holds only under the policy that required it: same rule and profile, a
// decider who is still an approver, and never the requester.
func TestApprovalIsCheckedAgainstPolicyWhenUsed(t *testing.T) {
	for name, tt := range map[string]struct {
		change func(d *core.Decision, a *store.Approval)
		want   string
	}{
		"rule changed":    {func(d *core.Decision, _ *store.Approval) { d.Rule = "new-strict" }, "policy"},
		"profile changed": {func(d *core.Decision, _ *store.Approval) { d.Profile = "team-a" }, "profile"},
		"approver removed": {func(d *core.Decision, _ *store.Approval) {
			d.Approvers = &core.Approvers{Users: []string{"cto@example.com"}}
		}, "no longer an approver"},
		"decided by requester": {func(_ *core.Decision, a *store.Approval) {
			a.DecidedBy, a.DecidedGroups = alice.Subject, []string{"platform"}
		}, "own request"},
	} {
		t.Run(name, func(t *testing.T) {
			st := &scaleTool{diff: "d"}
			ap := &memApprovals{}
			dec := core.Decision{Effect: core.EffectApprove, Rule: "b", Approvers: &core.Approvers{Groups: []string{"platform"}}}
			e, err := New(Options{
				Tools: []Tool{st.tool()}, Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
				Authorizer: authorizerFunc(func(core.Request) core.Decision { return dec }),
				Auditor:    &recordingAuditor{}, Approvals: ap,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.Invoke(context.Background(), alice, "scale", json.RawMessage(`{}`))
			var req *ApprovalRequiredError
			if !errors.As(err, &req) {
				t.Fatal(err)
			}
			ap.set(req.ID, func(a *store.Approval) {
				a.Status, a.DecidedBy, a.DecidedGroups = store.ApprovalApproved, "lead@example.com", []string{"platform"}
				tt.change(&dec, a)
			})
			_, err = e.Invoke(context.Background(), alice, "scale", json.RawMessage(`{"approval":"`+req.ID+`"}`))
			if err == nil || !strings.Contains(err.Error(), tt.want) || st.ran != 0 {
				t.Fatalf("err = %v, ran %d", err, st.ran)
			}
		})
	}
}

// The outcome records what the write did, as its Run reports it, not only the preview.
func TestOutcomeRecordsTheAppliedChange(t *testing.T) {
	tl := Tool{
		Name: "w", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: map[string]any{},
				Preview: func(context.Context, string) (*core.Change, error) { return &core.Change{Diff: "previewed"}, nil },
				Run:     func(context.Context, string) (any, error) { return &core.Change{Diff: "applied"}, nil },
			}, nil
		},
	}
	aud := &recordingAuditor{}
	e, _ := New(Options{Tools: []Tool{tl}, Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}}, Authorizer: authorizerFunc(allowAll), Auditor: aud})
	if _, err := e.Invoke(context.Background(), alice, "w", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if aud.records[0].Change.Diff != "previewed" || aud.records[1].Change.Diff != "applied" {
		t.Fatalf("intent %q, outcome %q", aud.records[0].Change.Diff, aud.records[1].Change.Diff)
	}
}

// One requester cannot flood approvers: past a number of pending requests, new ones are
// refused until some are decided or expire.
func TestPendingApprovalsAreCapped(t *testing.T) {
	st := &scaleTool{diff: "d"}
	e := approvalEngine(t, st, core.EffectApprove, &recordingAuditor{}, &memApprovals{})
	for i := range MaxPendingApprovals {
		_, err := e.Invoke(context.Background(), alice, "scale", json.RawMessage(fmt.Sprintf(`{"replicas":%d}`, i)))
		var req *ApprovalRequiredError
		if !errors.As(err, &req) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := e.Invoke(context.Background(), alice, "scale", json.RawMessage(`{"replicas":999}`))
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("err = %v", err)
	}
}

// A tool whose risk depends on the call (propose_change: read when it only suggests,
// write when it opens a pull request) is authorized and audited with the call's risk.
func TestPreparedRiskOverridesTheTools(t *testing.T) {
	var risks []core.Risk
	tl := Tool{
		Name: "propose", Risk: core.RiskWrite, Risks: []core.Risk{core.RiskRead}, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(raw json.RawMessage) (*Prepared, error) {
			var in struct{ Suggest bool }
			_ = json.Unmarshal(raw, &in)
			p := &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: map[string]any{},
				Run: func(context.Context, string) (any, error) { return "ok", nil },
			}
			if in.Suggest {
				p.Risk = core.RiskRead
			}
			return p, nil
		},
	}
	aud := &recordingAuditor{}
	e, _ := New(Options{
		Tools: []Tool{tl}, Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer: authorizerFunc(func(r core.Request) core.Decision {
			risks = append(risks, r.Risk)
			return core.Decision{Effect: core.EffectAllow, Rule: "b"}
		}),
		Auditor: aud,
	})
	if _, err := e.Invoke(context.Background(), alice, "propose", json.RawMessage(`{"Suggest":true}`)); err != nil {
		t.Fatal(err)
	}
	if risks[0] != core.RiskRead || len(aud.records) != 1 || aud.records[0].Risk != core.RiskRead {
		t.Fatalf("suggest: risks %v, records %d", risks, len(aud.records))
	}
	if _, err := e.Invoke(context.Background(), alice, "propose", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if risks[1] != core.RiskWrite || len(aud.records) != 3 || aud.records[1].Phase != audit.PhaseIntent {
		t.Fatalf("write: risks %v, records %+v", risks, aud.records)
	}
}

// A write's Run knows the id of its intent record, so what it creates (a pull request)
// can point back to the audit.
func TestRunSeesTheIntentRecordID(t *testing.T) {
	var seen string
	tl := Tool{
		Name: "propose", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: map[string]any{},
				Run: func(ctx context.Context, _ string) (any, error) {
					seen, _ = core.AuditIDFrom(ctx)
					return "ok", nil
				},
			}, nil
		},
	}
	aud := &recordingAuditor{}
	e, _ := New(Options{
		Tools: []Tool{tl}, Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer: authorizerFunc(func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow, Rule: "b"} }),
		Auditor:    aud,
	})
	if _, err := e.Invoke(context.Background(), alice, "propose", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if seen == "" || aud.records[0].Phase != audit.PhaseIntent || seen != aud.records[0].ID {
		t.Fatalf("Run saw %q, intent record %+v", seen, aud.records[0])
	}
}

// A call may lower its risk only to one its tool declares: policy and rendered RBAC were
// derived from those.
func TestPreparedRiskMustBeDeclared(t *testing.T) {
	var risks []core.Risk
	tl := Tool{
		Name: "propose", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: map[string]any{},
				Run:  func(context.Context, string) (any, error) { return "ok", nil },
				Risk: core.RiskRead,
			}, nil
		},
	}
	e, _ := New(Options{
		Tools: []Tool{tl}, Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer: authorizerFunc(func(r core.Request) core.Decision {
			risks = append(risks, r.Risk)
			return core.Decision{Effect: core.EffectAllow, Rule: "b"}
		}),
		Auditor: &recordingAuditor{},
	})
	if _, err := e.Invoke(context.Background(), alice, "propose", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if risks[0] != core.RiskWrite {
		t.Fatalf("an undeclared lower risk was taken: %v", risks)
	}
}

// Calls nobody waits on (an alert investigation, an agent) cannot become approval
// requests: an approve decision denies them, and no request is stored.
func TestUnattendedCallsAreNotApproved(t *testing.T) {
	tl := Tool{
		Name: "peek", Risk: core.RiskRead, InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*Prepared, error) {
			return &Prepared{
				Scope: core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}, Args: map[string]any{},
				Run: func(context.Context, string) (any, error) { return "ok", nil },
			}, nil
		},
	}
	store := &memApprovals{}
	e, _ := New(Options{
		Tools: []Tool{tl}, Environments: []Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer: authorizerFunc(func(core.Request) core.Decision {
			return core.Decision{Effect: core.EffectApprove, Rule: "careful", Approvers: &core.Approvers{Groups: []string{"platform"}}}
		}),
		Auditor: &recordingAuditor{}, Approvals: store,
	})
	_, err := e.Invoke(core.WithUnattended(context.Background()), alice, "peek", json.RawMessage(`{}`))
	var denied *DeniedError
	if !errors.As(err, &denied) || !strings.Contains(err.Error(), "approval") || len(store.m) != 0 {
		t.Fatalf("err = %v, stored %d", err, len(store.m))
	}
}
