package policy

import (
	"slices"

	"github.com/quyendv/opsward/internal/core"
)

// ReachableProfiles returns, sorted, the profiles of the allow bindings that could allow
// req's tool within req's scope: their subjects and role match, and their environment and
// namespace patterns cover the scope. A binding without a profile counts as the reader's.
// Selectors narrower than the scope (workloads and apps on a scope without a workload)
// and conditions are not consulted. A deny or approval without a condition or narrower
// selectors that covers the whole scope makes it unreachable: nil. docs/design/0006.
func (p *Policy) ReachableProfiles(req core.Request) []string {
	var out []string
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
			if req.Scope.Namespace == "" || anyGlob(s.Namespaces, req.Scope.Namespace) {
				profile := b.Profile
				if profile == "" {
					profile = core.ProfileReader
				}
				out = append(out, profile)
			}
		case core.EffectDeny, core.EffectApprove:
			// A pattern that matches "" (such as *) covers every namespace.
			whole := b.When == "" && len(s.Workloads) == 0 && len(s.Apps) == 0 &&
				anyGlob(s.Namespaces, req.Scope.Namespace)
			if whole {
				return nil
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
