package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
)

func TestScaleWorkloadPreviewsThenApplies(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{}, change: &capability.WorkloadChange{Diff: "spec.replicas: 2 -> 5", Previous: map[string]any{"spec.replicas": int32(2)}}}
	tl, err := ScaleWorkload(src)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Risk != core.RiskWrite || !slices.Contains(tl.Args, "approval") {
		t.Fatalf("tool = %+v", tl)
	}
	p, err := tl.Prepare(json.RawMessage(`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","replicas":5}`))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Preview(context.Background(), "operator")
	if err != nil || ch.Diff != "spec.replicas: 2 -> 5" || ch.Strategy != "direct" || ch.Previous["spec.replicas"] != int32(2) {
		t.Fatalf("preview = %+v, %v", ch, err)
	}
	if _, err := p.Run(context.Background(), "operator"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(src.dryRuns, []bool{true, false}) || src.replicas != 5 {
		t.Fatalf("dry runs %v, replicas %d", src.dryRuns, src.replicas)
	}
	for _, bad := range []string{
		`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","replicas":-1}`,
		`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","replicas":1001}`,
		`{"environment":"staging","namespace":"shop","kind":"DaemonSet","name":"api","replicas":1}`,
	} {
		if _, err := tl.Prepare(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestRestartWorkloadNotesTheReconciler(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{}, change: &capability.WorkloadChange{Diff: "restart", Reconciler: "Argo CD"}}
	tl, _ := RestartWorkload(src)
	p, err := tl.Prepare(json.RawMessage(`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}`))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Preview(context.Background(), "operator")
	if err != nil || !strings.Contains(ch.Note, "Argo CD") {
		t.Fatalf("preview = %+v, %v", ch, err)
	}
}

// memApprovalStore is an in-memory approval store for tool tests.
type memApprovalStore struct{ m map[string]store.Approval }

func (s *memApprovalStore) Approval(_ context.Context, id string) (store.Approval, bool, error) {
	a, ok := s.m[id]
	return a, ok, nil
}

func (s *memApprovalStore) ListApprovals(context.Context, time.Time) ([]store.Approval, error) {
	var out []store.Approval
	for _, a := range s.m {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b store.Approval) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *memApprovalStore) DecideApproval(_ context.Context, id, status, by, reason string, at time.Time) (bool, error) {
	a := s.m[id]
	if a.Status != store.ApprovalPending {
		return false, nil
	}
	a.Status, a.DecidedBy, a.Reason, a.DecidedAt = status, by, reason, at
	s.m[id] = a
	return true, nil
}

func pending(id, requester string) store.Approval {
	return store.Approval{
		ID: id, Requester: requester, Tool: "scale_workload", Diff: "d", ApproverGroups: []string{"platform"},
		Status: store.ApprovalPending, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestDecideApproval(t *testing.T) {
	st := &memApprovalStore{m: map[string]store.Approval{"r1": pending("r1", "alice@example.com"), "r2": pending("r2", "lead@example.com")}}
	tl, err := DecideApproval(st)
	if err != nil {
		t.Fatal(err)
	}
	if !tl.Global || tl.Risk != core.RiskWrite {
		t.Fatalf("tool = %+v", tl)
	}
	lead := core.Principal{Subject: "lead@example.com", Groups: []string{"platform"}}
	call := func(p core.Principal, args string) error {
		prep, err := tl.Prepare(json.RawMessage(args))
		if err != nil {
			return err
		}
		_, err = prep.Run(core.WithPrincipal(context.Background(), p), "")
		return err
	}
	for name, tt := range map[string]struct {
		p    core.Principal
		args string
		want string
	}{
		"not an approver": {core.Principal{Subject: "bob@example.com", Groups: []string{"team-a"}}, `{"id":"r1","decision":"approve"}`, "not an approver"},
		"own request":     {lead, `{"id":"r2","decision":"approve"}`, "own request"},
		"unknown":         {lead, `{"id":"r9","decision":"approve"}`, "unknown"},
	} {
		if err := call(tt.p, tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := call(lead, `{"id":"r1","decision":"approve","reason":"ok"}`); err != nil {
		t.Fatal(err)
	}
	if a := st.m["r1"]; a.Status != store.ApprovalApproved || a.DecidedBy != "lead@example.com" || a.Reason != "ok" {
		t.Fatalf("decided = %+v", a)
	}
	if err := call(lead, `{"id":"r1","decision":"deny"}`); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("decided twice: %v", err)
	}
}

func TestListApprovalsShowsOwnAndDecidable(t *testing.T) {
	st := &memApprovalStore{m: map[string]store.Approval{
		"r1": pending("r1", "alice@example.com"),
		"r2": pending("r2", "carol@example.com"),
	}}
	r3 := pending("r3", "carol@example.com")
	r3.ApproverGroups = []string{"other"}
	st.m["r3"] = r3
	tl, _ := ListApprovals(st)
	prep, err := tl.Prepare(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := prep.Run(core.WithPrincipal(context.Background(), core.Principal{Subject: "alice@example.com", Groups: []string{"platform"}}), "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"r1"`) || !strings.Contains(string(b), `"r2"`) || strings.Contains(string(b), `"r3"`) {
		t.Fatalf("approvals = %s", b)
	}
}
