package policy

import "github.com/quyendv/opsward/internal/core"

// BindingView is a binding as shown to a principal it names, with its role spelled out.
type BindingView struct {
	Binding
	Risk  []core.Risk
	Tools []string
}

// Naming returns the bindings whose subjects include p, in policy order, and those
// that name p as an approver. The console shows a person what policy says about them
// (docs/design/0015).
func (p *Policy) Naming(pr core.Principal) (grants, approves []BindingView) {
	for i := range p.bindings {
		b := &p.bindings[i]
		v := BindingView{Binding: b.Binding, Risk: b.role.Risk, Tools: b.role.Tools}
		if b.Subjects.match(pr) {
			grants = append(grants, v)
		}
		a := &core.Approvers{Users: b.Approvers.Users, Groups: b.Approvers.Groups}
		if b.Effect == core.EffectApprove && a.Includes(pr) {
			approves = append(approves, v)
		}
	}
	return grants, approves
}

// MayAllow reports whether an allow binding names p and grants it the tool, whatever its
// scope: a caller it does not is denied an Exact listing outright (docs/design/0015).
func (p *Policy) MayAllow(req core.Request) bool {
	for i := range p.bindings {
		b := &p.bindings[i]
		if b.Effect == core.EffectAllow && b.Subjects.match(req.Principal) && b.role.contains(req.Tool, req.Risk) {
			return true
		}
	}
	return false
}
