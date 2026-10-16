package storetest

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

func approval(id string, created time.Time) store.Approval {
	return store.Approval{
		ID: id, Requester: "alice@example.com", Tool: "scale_workload",
		Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "api",
		ArgsHash: "sha256:abc", Diff: "spec.replicas: 3 -> 5", Rule: "team-a-scale", Profile: "operator",
		ApproverUsers: []string{"lead@example.com"}, ApproverGroups: []string{"platform"},
		Status: store.ApprovalPending, CreatedAt: created, ExpiresAt: created.Add(time.Hour),
	}
}

// Approvals checks approval requests: create, read, list, decide once, use once.
func Approvals(t *testing.T, open func(t *testing.T) store.Store) {
	ctx := context.Background()

	t.Run("round trip", func(t *testing.T) {
		s := open(t)
		want := approval("r1", t0)
		if err := s.CreateApproval(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Approval(ctx, "r1")
		if err != nil || !ok {
			t.Fatalf("Approval = %v, %v", ok, err)
		}
		if got.Requester != want.Requester || got.Tool != want.Tool || got.Name != "api" || got.ArgsHash != want.ArgsHash ||
			got.Diff != want.Diff || got.Rule != want.Rule || got.Profile != want.Profile || !slices.Equal(got.ApproverGroups, want.ApproverGroups) || !slices.Equal(got.ApproverUsers, want.ApproverUsers) ||
			got.Status != store.ApprovalPending || !got.CreatedAt.Equal(t0) || !got.ExpiresAt.Equal(t0.Add(time.Hour)) || !got.DecidedAt.IsZero() {
			t.Fatalf("Approval = %+v", got)
		}
		if _, ok, _ := s.Approval(ctx, "missing"); ok {
			t.Fatal("missing approval found")
		}
	})

	t.Run("decide only a pending request", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("r1", t0))
		ok, err := s.DecideApproval(ctx, "r1", store.ApprovalApproved, "lead@example.com", []string{"platform"}, "fine", t0.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("DecideApproval = %v, %v", ok, err)
		}
		if ok, _ := s.DecideApproval(ctx, "r1", store.ApprovalDenied, "other@example.com", nil, "", t0.Add(2*time.Minute)); ok {
			t.Fatal("a decided request was decided again")
		}
		got, _, _ := s.Approval(ctx, "r1")
		if got.Status != store.ApprovalApproved || got.DecidedBy != "lead@example.com" || !slices.Equal(got.DecidedGroups, []string{"platform"}) ||
			got.Reason != "fine" || !got.DecidedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("decided = %+v", got)
		}
	})

	t.Run("use only an approved request, once", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("r1", t0))
		if ok, _ := s.UseApproval(ctx, "r1", t0); ok {
			t.Fatal("a pending request was used")
		}
		_, _ = s.DecideApproval(ctx, "r1", store.ApprovalApproved, "lead@example.com", nil, "", t0)
		var wg sync.WaitGroup
		used := make(chan bool, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.UseApproval(ctx, "r1", t0.Add(time.Minute))
				if err != nil {
					t.Error(err)
				}
				used <- ok
			}()
		}
		wg.Wait()
		close(used)
		n := 0
		for ok := range used {
			if ok {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("used %d times", n)
		}
		got, _, _ := s.Approval(ctx, "r1")
		if got.Status != store.ApprovalUsed || !got.UsedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("used = %+v", got)
		}
	})

	// Expiry holds at the moment of the write, not only when a caller checked it.
	t.Run("expired requests cannot be decided or used", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("r1", t0))
		late := t0.Add(2 * time.Hour)
		if ok, _ := s.DecideApproval(ctx, "r1", store.ApprovalApproved, "lead@example.com", nil, "", late); ok {
			t.Fatal("decided after expiry")
		}
		_, _ = s.DecideApproval(ctx, "r1", store.ApprovalApproved, "lead@example.com", nil, "", t0)
		if ok, _ := s.UseApproval(ctx, "r1", late); ok {
			t.Fatal("used after expiry")
		}
	})

	t.Run("list newest first since", func(t *testing.T) {
		s := open(t)
		for i, id := range []string{"r1", "r2", "r3"} {
			_ = s.CreateApproval(ctx, approval(id, t0.Add(time.Duration(i)*time.Hour)))
		}
		list, err := s.ListApprovals(ctx, t0.Add(30*time.Minute), 10)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		if !slices.Equal(ids, []string{"r3", "r2"}) {
			t.Fatalf("ListApprovals = %v", ids)
		}
		if list, _ := s.ListApprovals(ctx, t0, 1); len(list) != 1 || list[0].ID != "r3" {
			t.Fatalf("limit: %+v", list)
		}
	})

	t.Run("count pending of a requester", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("r1", t0))
		_ = s.CreateApproval(ctx, approval("r2", t0))
		other := approval("r3", t0)
		other.Requester = "bob@example.com"
		_ = s.CreateApproval(ctx, other)
		_, _ = s.DecideApproval(ctx, "r2", store.ApprovalApproved, "lead@example.com", nil, "", t0)
		if n, err := s.CountPendingApprovals(ctx, "alice@example.com", t0); err != nil || n != 1 {
			t.Fatalf("pending = %d, %v", n, err)
		}
		if n, _ := s.CountPendingApprovals(ctx, "alice@example.com", t0.Add(2*time.Hour)); n != 0 {
			t.Fatalf("expired requests are not pending: %d", n)
		}
	})

	// An approver can take back an approval until it is used.
	t.Run("withdraw an approved request", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("r1", t0))
		if ok, _ := s.WithdrawApproval(ctx, "r1", "lead@example.com", nil, "no", t0); ok {
			t.Fatal("withdrew a pending request")
		}
		_, _ = s.DecideApproval(ctx, "r1", store.ApprovalApproved, "lead@example.com", nil, "", t0)
		if ok, err := s.WithdrawApproval(ctx, "r1", "cto@example.com", []string{"platform"}, "rollback first", t0.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("WithdrawApproval = %v, %v", ok, err)
		}
		got, _, _ := s.Approval(ctx, "r1")
		if got.Status != store.ApprovalDenied || got.DecidedBy != "cto@example.com" || got.Reason != "rollback first" {
			t.Fatalf("withdrawn = %+v", got)
		}
		if ok, _ := s.UseApproval(ctx, "r1", t0.Add(2*time.Minute)); ok {
			t.Fatal("a withdrawn approval was used")
		}
	})

	t.Run("prune", func(t *testing.T) {
		s := open(t)
		_ = s.CreateApproval(ctx, approval("old", t0))
		_ = s.CreateApproval(ctx, approval("new", t0.Add(48*time.Hour)))
		n, err := s.PruneApprovals(ctx, t0.Add(24*time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("PruneApprovals = %d, %v", n, err)
		}
		if _, ok, _ := s.Approval(ctx, "new"); !ok {
			t.Fatal("pruned a live request")
		}
	})
}
