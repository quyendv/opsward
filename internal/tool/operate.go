package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/store"
)

const maxReplicas = 1000

// The approval argument of a retried call is the engine's (engine.ApprovalArg).
type scaleInput struct {
	workloadInput
	Replicas int `json:"replicas" jsonschema:"desired number of replicas"`
}

// change turns a provider's change into the one engine and audit record.
func change(c *capability.WorkloadChange) *core.Change {
	out := &core.Change{Strategy: "direct", Diff: c.Diff, Previous: c.Previous}
	if c.Reconciler != "" {
		out.Note = c.Reconciler + " manages this workload; if it reverts pod template annotations it will roll it again"
	}
	return out
}

// writePrepared is workloadPrepared for a narrow change: the same call previews with a
// dry run and applies.
func writePrepared(src capability.WorkloadSource, v workloadInput, obj map[string]any,
	do func(context.Context, capability.Workloads, capability.WorkloadRef, bool) (*capability.WorkloadChange, error),
) *engine.Prepared {
	p := workloadPrepared(src, v, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
		c, err := do(ctx, w, ref, false)
		if err != nil {
			return nil, err
		}
		return change(c), nil
	})
	ref := capability.WorkloadRef{Namespace: v.Namespace, Kind: v.Kind, Name: v.Name}
	p.Preview = func(ctx context.Context, profile string) (*core.Change, error) {
		w, err := src.Workloads(v.Environment, profile)
		if err != nil {
			return nil, err
		}
		c, err := do(ctx, w, ref, true)
		if err != nil {
			return nil, err
		}
		return change(c), nil
	}
	return p
}

// RestartWorkload is restart_workload.
func RestartWorkload(src capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[workloadInput](refineWorkloadInput)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "restart_workload",
		Description: "Restart a workload's pods through its rollout, as kubectl rollout restart does. Previewed first; " +
			"may need approval, in which case call again with the approval id once approved.",
		Risk: core.RiskWrite, InputSchema: in.schema, Args: in.names,
		Needs: []string{capability.OpWorkloadsGet, capability.OpWorkloadsRestart},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return writePrepared(src, v, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef, dry bool) (*capability.WorkloadChange, error) {
				return w.RestartWorkload(ctx, ref, dry)
			}), nil
		},
	}, nil
}

// ScaleWorkload is scale_workload.
func ScaleWorkload(src capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[scaleInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["kind"].Enum = []any{"Deployment", "StatefulSet"}
		s.Properties["replicas"].Minimum = jsonschema.Ptr(0.0)
		s.Properties["replicas"].Maximum = jsonschema.Ptr(float64(maxReplicas))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "scale_workload",
		Description: "Set a Deployment's or StatefulSet's replicas. Refused when a reconciler (Argo CD, Flux, Helm) " +
			"manages it or an autoscaler targets it. Previewed first; may need approval.",
		Risk: core.RiskWrite, InputSchema: in.schema, Args: in.names,
		Needs: []string{capability.OpWorkloadsGet, capability.OpWorkloadsScale},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			replicas := int32(v.Replicas) //nolint:gosec // G115: bounded by the schema
			return writePrepared(src, v.workloadInput, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef, dry bool) (*capability.WorkloadChange, error) {
				return w.ScaleWorkload(ctx, ref, replicas, dry)
			}), nil
		},
	}, nil
}

// ApprovalStore is what the approval tools read and decide.
type ApprovalStore interface {
	Approval(ctx context.Context, id string) (store.Approval, bool, error)
	ListApprovals(ctx context.Context, since time.Time, limit int) ([]store.Approval, error)
	DecideApproval(ctx context.Context, id, status, by string, byGroups []string, reason string, at time.Time) (bool, error)
	WithdrawApproval(ctx context.Context, id, by string, byGroups []string, reason string, at time.Time) (bool, error)
}

// approvalListWindow is how far back list_approvals looks, and approvalListScan how
// many of the newest requests it reads; requests are capped per requester, so this covers
// a busy day.
const (
	approvalListWindow = 24 * time.Hour
	approvalListScan   = 1000
)

// ApprovalView is an approval request as the approval tools show it.
type ApprovalView struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Requester string          `json:"requester"`
	Tool      string          `json:"tool"`
	Scope     core.Scope      `json:"scope"`
	Diff      string          `json:"diff"`
	Approvers *core.Approvers `json:"approvers"`
	ExpiresAt time.Time       `json:"expiresAt"`
	DecidedBy string          `json:"decidedBy,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	// CanDecide is true when the caller may approve or deny it now.
	CanDecide bool `json:"canDecide"`
}

func view(a store.Approval, p core.Principal, now time.Time) ApprovalView {
	approvers := &core.Approvers{Users: a.ApproverUsers, Groups: a.ApproverGroups}
	return ApprovalView{
		ID: a.ID, Status: a.Status, Requester: a.Requester, Tool: a.Tool,
		Scope: core.Scope{Environment: a.Environment, Namespace: a.Namespace, Kind: a.Kind, Name: a.Name},
		Diff:  a.Diff, Approvers: approvers, ExpiresAt: a.ExpiresAt, DecidedBy: a.DecidedBy, Reason: a.Reason,
		CanDecide: a.Status == store.ApprovalPending && now.Before(a.ExpiresAt) && approvers.Includes(p) &&
			a.Requester != core.NormalizeSubject(p.Subject),
	}
}

// ListApprovals is list_approvals: the caller's own requests and those it may decide,
// from the last day.
func ListApprovals(st ApprovalStore) (engine.Tool, error) {
	in, err := newInput[struct{}](nil)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name:        "list_approvals",
		Description: "List approval requests from the last day that you made or may decide, with the change each one makes.",
		Risk:        core.RiskRead, InputSchema: in.schema, Args: in.names, Global: true, NeverApproved: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			_, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{Args: obj, Run: func(ctx context.Context, _ string) (any, error) {
				p, _ := core.PrincipalFrom(ctx)
				now := time.Now()
				all, err := st.ListApprovals(ctx, now.Add(-approvalListWindow), approvalListScan)
				if err != nil {
					return nil, err
				}
				out := []ApprovalView{}
				for _, a := range all {
					approvers := &core.Approvers{Users: a.ApproverUsers, Groups: a.ApproverGroups}
					if a.Requester == core.NormalizeSubject(p.Subject) || approvers.Includes(p) {
						out = append(out, view(a, p, now))
					}
				}
				return out, nil
			}}, nil
		},
	}, nil
}

type decideInput struct {
	ID       string `json:"id" jsonschema:"approval request id"`
	Decision string `json:"decision" jsonschema:"approve or deny a pending request; withdraw an approval that has not been used yet"`
	Reason   string `json:"reason,omitempty" jsonschema:"why, recorded with the decision"`
}

// DecideApproval is decide_approval.
func DecideApproval(st ApprovalStore) (engine.Tool, error) {
	in, err := newInput[decideInput](func(s *jsonschema.Schema) {
		s.Properties["id"].MinLength = jsonschema.Ptr(1)
		s.Properties["decision"].Enum = []any{"approve", "deny", "withdraw"}
		s.Properties["reason"].MaxLength = jsonschema.Ptr(1024)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "decide_approval",
		Description: "Approve or deny an approval request that you may decide (see list_approvals), or withdraw an " +
			"approval before it is used. You cannot decide your own request. The requester then calls again with the " +
			"approval id.",
		Risk: core.RiskWrite, InputSchema: in.schema, Args: in.names, Global: true, NeverApproved: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{Args: obj, Run: func(ctx context.Context, _ string) (any, error) {
				p, _ := core.PrincipalFrom(ctx)
				a, ok, err := st.Approval(ctx, v.ID)
				if err != nil {
					return nil, err
				}
				now := time.Now()
				approvers := &core.Approvers{Users: a.ApproverUsers, Groups: a.ApproverGroups}
				switch {
				case !ok:
					return nil, core.InvalidInput(fmt.Sprintf("unknown approval request %q", v.ID))
				case a.Requester == core.NormalizeSubject(p.Subject):
					return nil, core.InvalidInput("an approver cannot decide their own request")
				case !approvers.Includes(p):
					return nil, core.InvalidInput(fmt.Sprintf("you are not an approver of request %s", v.ID))
				case !now.Before(a.ExpiresAt):
					return nil, core.InvalidInput(fmt.Sprintf("approval request %s has expired", v.ID))
				}
				by := core.NormalizeSubject(p.Subject)
				var status string
				var done bool
				if v.Decision == "withdraw" {
					status = store.ApprovalDenied
					if done, err = st.WithdrawApproval(ctx, v.ID, by, p.Groups, v.Reason, now.UTC()); err != nil {
						return nil, err
					}
					if !done {
						return nil, core.InvalidInput(fmt.Sprintf("approval request %s is not an approval that can still be withdrawn: not approved, already used, or expired", v.ID))
					}
				} else {
					status = store.ApprovalApproved
					if v.Decision == "deny" {
						status = store.ApprovalDenied
					}
					if done, err = st.DecideApproval(ctx, v.ID, status, by, p.Groups, v.Reason, now.UTC()); err != nil {
						return nil, err
					}
					if !done {
						return nil, core.InvalidInput(fmt.Sprintf("approval request %s has already been decided, or has expired", v.ID))
					}
				}
				a.Status, a.DecidedBy, a.Reason = status, core.NormalizeSubject(p.Subject), v.Reason
				return view(a, p, now), nil
			}}, nil
		},
	}, nil
}
