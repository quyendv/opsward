package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
)

// ApprovalArg is the argument that names an approved request when a call is retried.
const ApprovalArg = "approval"

// DefaultApprovalTTL is how long a request can be decided and used.
const DefaultApprovalTTL = time.Hour

// MaxPendingApprovals bounds a requester's pending requests, so that one principal, or
// an agent in a loop, cannot flood approvers.
const MaxPendingApprovals = 20

// Approvals stores approval requests (docs/design/0007). store.Store implements it.
type Approvals interface {
	CreateApproval(ctx context.Context, a store.Approval) error
	Approval(ctx context.Context, id string) (store.Approval, bool, error)
	UseApproval(ctx context.Context, id string, at time.Time) (bool, error)
	CountPendingApprovals(ctx context.Context, requester string, now time.Time) (int, error)
}

// ApprovalRequiredError answers a call that needs approval: it was not run, and became
// the request ID.
type ApprovalRequiredError struct {
	ID        string
	Diff      string
	Approvers *core.Approvers
	ExpiresAt time.Time
}

func (e *ApprovalRequiredError) Error() string {
	var who []string
	if e.Approvers != nil {
		who = append(who, e.Approvers.Users...)
		for _, g := range e.Approvers.Groups {
			who = append(who, "group "+g)
		}
	}
	return fmt.Sprintf("approval required: the call was not run and became approval request %s, valid until %s. "+
		"Change: %s. Ask an approver (%s) to approve it with decide_approval, then call again with the same arguments "+
		"and %s: %q.", e.ID, e.ExpiresAt.UTC().Format(time.RFC3339), e.Diff, strings.Join(who, ", "), ApprovalArg, e.ID)
}

// ApprovalError refuses a retry whose approval does not hold.
type ApprovalError struct{ msg string }

func (e *ApprovalError) Error() string { return e.msg }

func approvalErr(format string, a ...any) error {
	return &ApprovalError{msg: fmt.Sprintf(format, a...)}
}

// splitApproval takes the approval argument out of a call's arguments: it belongs to the
// engine, so every tool can be retried with one, and it is not part of what the tool,
// policy or the argument hash see.
func splitApproval(raw json.RawMessage) (json.RawMessage, string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw, "", nil // not an object: the tool's own validation reports it
	}
	v, ok := obj[ApprovalArg]
	if !ok {
		return raw, "", nil
	}
	var id string
	if err := json.Unmarshal(v, &id); err != nil {
		return nil, "", fmt.Errorf("%s must be a string", ApprovalArg)
	}
	delete(obj, ApprovalArg)
	rest, err := json.Marshal(obj)
	if err != nil {
		return nil, "", err
	}
	return rest, id, nil
}

// withApprovalArg returns a copy of schema that offers the approval argument, so that a
// client validating arguments lets a retry through.
func withApprovalArg(schema *jsonschema.Schema) *jsonschema.Schema {
	s := *schema
	s.Properties = maps.Clone(schema.Properties)
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{}
	}
	s.Properties[ApprovalArg] = &jsonschema.Schema{
		Type:        "string",
		Description: "only when calling again after a call needed approval: the id of the approved request",
	}
	return &s
}

// argsHash identifies a call's arguments, without the approval that may accompany a retry.
func argsHash(args map[string]any) string {
	m := maps.Clone(args)
	delete(m, ApprovalArg)
	b, err := json.Marshal(m) // map keys are sorted
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (e *Engine) preview(ctx context.Context, prep *Prepared, profile string) (*core.Change, error) {
	if prep.Preview == nil {
		return &core.Change{Strategy: "direct", Diff: "(no preview)"}, nil
	}
	return prep.Preview(ctx, profile)
}

// requestApproval stores a request for a call that needs approval, with its preview,
// and answers with it instead of running the call.
func (e *Engine) requestApproval(ctx context.Context, p core.Principal, tool string, prep *Prepared, rec *audit.Record, dec *core.Decision,
	profile string, now time.Time, finish func(string, error) error,
) (*Result, error) {
	pending, err := e.approvals.CountPendingApprovals(ctx, core.NormalizeSubject(p.Subject), now)
	if err != nil {
		return nil, finish(audit.StatusError, err)
	}
	if pending >= MaxPendingApprovals {
		return nil, finish(audit.StatusDenied, approvalErr("%d of your approval requests are pending: wait until some are decided or expire", pending))
	}
	change, err := e.preview(ctx, prep, profile)
	if err != nil {
		status := audit.StatusError
		if isInvalid(err) {
			status = audit.StatusInvalid
		}
		return nil, finish(status, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, finish(audit.StatusError, err)
	}
	ttl := e.approvalTTL
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	a := store.Approval{
		ID: id.String(), Requester: core.NormalizeSubject(p.Subject), Tool: tool,
		Environment: prep.Scope.Environment, Namespace: prep.Scope.Namespace, Kind: prep.Scope.Kind, Name: prep.Scope.Name,
		ArgsHash: argsHash(prep.Args), Diff: change.Diff, Rule: dec.Rule, Profile: profile, Status: store.ApprovalPending,
		CreatedAt: now.UTC(), ExpiresAt: now.Add(ttl).UTC(),
	}
	if dec.Approvers != nil {
		a.ApproverUsers, a.ApproverGroups = dec.Approvers.Users, dec.Approvers.Groups
	}
	if err := e.approvals.CreateApproval(context.WithoutCancel(ctx), a); err != nil {
		return nil, finish(audit.StatusError, fmt.Errorf("storing the approval request: %w", err))
	}
	dec.Approval, rec.Change = a.ID, change
	return nil, finish(audit.StatusPending, &ApprovalRequiredError{ID: a.ID, Diff: a.Diff, Approvers: dec.Approvers, ExpiresAt: a.ExpiresAt})
}

// useApproval checks that a retried call carries an approval that holds for it under the
// policy in force now, and uses it: a request runs at most once. It returns the call's
// preview, and the audit status of a refusal.
func (e *Engine) useApproval(ctx context.Context, p core.Principal, tool string, prep *Prepared, id, profile string, dec core.Decision) (*core.Change, string, error) {
	a, ok, err := e.approvals.Approval(ctx, id)
	if err != nil {
		return nil, audit.StatusError, err
	}
	decider := core.Principal{Subject: a.DecidedBy, Groups: a.DecidedGroups}
	if refusal := checkApproval(a, ok, p, tool, prep, id, profile, dec, decider, e.now()); refusal != nil {
		return nil, audit.StatusDenied, refusal
	}
	change, err := e.preview(ctx, prep, profile)
	if err != nil {
		if isInvalid(err) {
			return nil, audit.StatusInvalid, err
		}
		return nil, audit.StatusError, err
	}
	if change.Diff != a.Diff {
		return nil, audit.StatusDenied, approvalErr("the target changed since approval request %s: approved %q, now %q; request approval again", id, a.Diff, change.Diff)
	}
	// Not cancellable: once consumed the call runs, and a client gone at this moment must
	// not leave it half done.
	used, err := e.approvals.UseApproval(context.WithoutCancel(ctx), id, e.now())
	if err != nil {
		return nil, audit.StatusError, err
	}
	if !used {
		return nil, audit.StatusDenied, approvalErr("approval request %s has already been used or has expired", id)
	}
	return change, "", nil
}

// checkApproval is everything about a stored approval that must hold before its call
// may run.
func checkApproval(a store.Approval, found bool, p core.Principal, tool string, prep *Prepared, id, profile string, dec core.Decision, decider core.Principal, now time.Time) error {
	switch {
	case !found:
		return approvalErr("unknown approval request %q", id)
	case a.Requester != core.NormalizeSubject(p.Subject):
		return approvalErr("approval request %s was made by someone else", id)
	case a.Tool != tool || a.ArgsHash != argsHash(prep.Args):
		return approvalErr("approval request %s was for other arguments; request approval again", id)
	case a.Status == store.ApprovalPending:
		return approvalErr("approval request %s has not been decided yet", id)
	case a.Status == store.ApprovalDenied:
		return approvalErr("approval request %s was denied by %s: %s", id, a.DecidedBy, a.Reason)
	case a.Status == store.ApprovalUsed:
		return approvalErr("approval request %s has already been used", id)
	case !now.Before(a.ExpiresAt):
		return approvalErr("approval request %s expired at %s; request approval again", id, a.ExpiresAt.Format(time.RFC3339))
	case a.Rule != dec.Rule:
		return approvalErr("the policy that required approval request %s changed (rule %s, now %s); request approval again", id, a.Rule, dec.Rule)
	case a.Profile != profile:
		return approvalErr("approval request %s was for profile %s, the call now runs with %s; request approval again", id, a.Profile, profile)
	case core.NormalizeSubject(a.DecidedBy) == a.Requester:
		return approvalErr("approval request %s was decided by its requester: an approver cannot approve their own request", id)
	case !dec.Approvers.Includes(decider):
		return approvalErr("%s, who approved request %s, is no longer an approver; request approval again", a.DecidedBy, id)
	}
	return nil
}

func isInvalid(err error) bool { return errors.Is(err, core.ErrInvalidInput) }
