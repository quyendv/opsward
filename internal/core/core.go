// Package core holds the vocabulary shared by the engine, policy and audit packages.
package core

import "time"

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

// Principal is who is calling.
type Principal struct {
	Subject string   `json:"subject"`
	Groups  []string `json:"groups,omitempty"`
	Adapter string   `json:"adapter"`
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

// Decision is the result of authorizing a Request.
type Decision struct {
	Effect  Effect `json:"effect"`
	Rule    string `json:"rule"`
	Profile string `json:"profile,omitempty"`
}
