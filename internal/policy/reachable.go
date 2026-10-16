package policy

import "github.com/quyendv/opsward/internal/core"

// Reachable reports whether some allow binding could allow req's tool within req's
// scope: its subjects and role match, its environment and namespace patterns cover the
// scope, and it uses the reader profile that listings read with (docs/design/0006).
// Selectors narrower than the scope (workloads and apps on a scope without a workload)
// and conditions are not consulted. A deny or approval without a condition or narrower
// selectors that covers the whole scope makes it unreachable.
func (p *Policy) Reachable(req core.Request) bool {
	reach := false
	for i := range p.bindings {
		b := &p.bindings[i]
		if !b.Subjects.match(req.Principal) || !b.role.contains(req.Tool, req.Risk) {
			continue
		}
		s := b.Scope
		if !anyGlob(s.Environments, req.Scope.Environment) {
			continue
		}
		switch b.Effect {
		case core.EffectAllow:
			readerProfile := b.Profile == "" || b.Profile == core.ProfileReader
			if readerProfile && (req.Scope.Namespace == "" || anyGlob(s.Namespaces, req.Scope.Namespace)) {
				reach = true
			}
		case core.EffectDeny, core.EffectApprove:
			// A pattern that matches "" (such as *) covers every namespace.
			whole := b.When == "" && len(s.Workloads) == 0 && len(s.Apps) == 0 &&
				anyGlob(s.Namespaces, req.Scope.Namespace)
			if whole {
				return false
			}
		}
	}
	return reach
}
