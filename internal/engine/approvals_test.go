package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	if !ok || a.Status != store.ApprovalApproved {
		return false, nil
	}
	a.Status, a.UsedAt = store.ApprovalUsed, at
	s.m[id] = a
	return true, nil
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
	ap.set(req.ID, func(a *store.Approval) { a.Status = store.ApprovalApproved; a.DecidedBy = "lead@example.com" })

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
