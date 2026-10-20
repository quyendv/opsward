// Package core holds the vocabulary shared by the engine, policy and audit packages.
package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Risk is the risk class a tool declares. It is never inferred from a call.
type Risk string

// Risk classes, from least to most dangerous.
const (
	RiskRead        Risk = "read"
	RiskWrite       Risk = "write"
	RiskDestructive Risk = "destructive"
)

// Valid reports whether r is one of the declared risk classes.
func (r Risk) Valid() bool {
	switch r {
	case RiskRead, RiskWrite, RiskDestructive:
		return true
	}
	return false
}

// Effect is the outcome of a policy decision.
type Effect string

// Policy effects. deny overrides approve, which overrides allow.
const (
	EffectAllow   Effect = "allow"
	EffectApprove Effect = "approve"
	EffectDeny    Effect = "deny"
)

// Valid reports whether e is one of the declared effects.
func (e Effect) Valid() bool {
	switch e {
	case EffectAllow, EffectApprove, EffectDeny:
		return true
	}
	return false
}

// Profiles a call runs with when the binding that authorized it names none: read tools
// use ProfileReader, every other tool ProfileOperator. ProfileReader also reads target
// labels for policy.
const (
	ProfileReader   = "reader"
	ProfileOperator = "operator"
)

// DefaultDenyRule names the decision taken when no binding matches.
const DefaultDenyRule = "default-deny"

// PerItemRule names the decision of a listing, whose items are authorized one by one.
const PerItemRule = "per-item"

// Principal is who is calling.
type Principal struct {
	Subject string   `json:"subject"`
	Groups  []string `json:"groups,omitempty"`
	Adapter string   `json:"adapter"`
	// Credential names what the caller authenticated with: "token:<id>" or "oidc".
	// Empty in local mode, and then absent from the JSON, so older records hash the same.
	Credential string `json:"credential,omitempty"`
}

// ErrInvalidInput matches errors caused by what the caller asked for, found while a call
// runs: a query outside its scope, a syntax error in a query. They are audited as
// invalid rather than as failures of the system.
var ErrInvalidInput = errors.New("invalid input")

// ErrForbidden matches a backend's refusal of a call policy allowed, as a cluster refuses
// the caller a profile acts as (docs/design/0014). They are audited as forbidden: the
// backend's decision, not Opsward's policy and not a failure of the system.
var ErrForbidden = errors.New("forbidden")

type invalidInputError struct{ err error }

func (e *invalidInputError) Error() string   { return e.err.Error() }
func (e *invalidInputError) Unwrap() []error { return []error{e.err, ErrInvalidInput} }

// InvalidInput returns an error with msg that matches ErrInvalidInput.
func InvalidInput(msg string) error { return &invalidInputError{errors.New(msg)} }

// AsInvalidInput marks err as caused by the caller's input.
func AsInvalidInput(err error) error { return &invalidInputError{err} }

// NormalizeSubject lowercases a subject that is an email address, so that policy and
// group membership do not depend on the case an issuer or an operator wrote it in.
// Other subjects, such as opaque IDs, are compared exactly.
func NormalizeSubject(s string) string {
	if strings.Contains(s, "@") {
		return strings.ToLower(s)
	}
	return s
}

// Scope is what a call targets, extracted from typed arguments.
type Scope struct {
	Environment string `json:"environment,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name,omitempty"`
	// Labels of the target, read by Opsward during validation. Used by policy, never
	// returned to the caller and not audited.
	Labels map[string]string `json:"-"`
	// LabelsKnown is false when the target's labels could not be read. Policy then
	// treats app-scoped bindings stricter than allow as matching.
	LabelsKnown bool `json:"-"`
}

// Request is everything policy sees about one call.
type Request struct {
	Principal Principal
	Tool      string
	Risk      Risk
	Scope     Scope
	Args      map[string]any
	Now       time.Time
}

// Note is a knowledge document attached to a tool result because the call matched it.
type Note struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// Decision is the result of authorizing a Request.
type Decision struct {
	Effect  Effect `json:"effect"`
	Rule    string `json:"rule"`
	Profile string `json:"profile,omitempty"`
	// Approvers may approve the call, for effect approve.
	Approvers *Approvers `json:"approvers,omitempty"`
	// Approval is the approval request a call ran under (docs/design/0007).
	Approval string `json:"approval,omitempty"`
}

// Approvers are the users and groups that may approve a call.
type Approvers struct {
	Users  []string `json:"users,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// Includes reports whether p is one of the approvers.
func (a *Approvers) Includes(p Principal) bool {
	if a == nil || p.Subject == "" {
		return false
	}
	subject := NormalizeSubject(p.Subject)
	for _, u := range a.Users {
		if NormalizeSubject(u) == subject {
			return true
		}
	}
	for _, g := range a.Groups {
		for _, pg := range p.Groups {
			if g == pg {
				return true
			}
		}
	}
	return false
}

// Change is what a write changed, or would change: the diff a dry run produced and the
// previous values of the fields it sets (docs/design/0007).
type Change struct {
	Strategy string         `json:"strategy"`
	Diff     string         `json:"diff"`
	Previous map[string]any `json:"previous,omitempty"`
	// Note says something the caller should know, such as a reconciler that may act.
	Note string `json:"note,omitempty"`
	// URL is where a proposed change can be reviewed, such as a pull request.
	URL string `json:"url,omitempty"`
}

type principalKey struct{}

// WithPrincipal returns ctx carrying the principal of the call it serves.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal a call runs for.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type auditIDKey struct{}

// WithAuditID returns ctx carrying the id of the intent record a write runs under.
func WithAuditID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, auditIDKey{}, id)
}

// AuditIDFrom returns the id of the intent record a write runs under, so that what the
// write creates can refer back to the audit.
func AuditIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(auditIDKey{}).(string)
	return id, ok && id != ""
}

type unattendedKey struct{}

// WithUnattended marks ctx as a call nobody waits on, such as an alert investigation or
// an agent's: it cannot become an approval request.
func WithUnattended(ctx context.Context) context.Context {
	return context.WithValue(ctx, unattendedKey{}, true)
}

// Unattended reports whether ctx is a call nobody waits on.
func Unattended(ctx context.Context) bool {
	u, _ := ctx.Value(unattendedKey{}).(bool)
	return u
}
