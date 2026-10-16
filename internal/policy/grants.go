package policy

import (
	"slices"

	"github.com/quyendv/opsward/internal/core"
)

// Grants returns, per environment of cat, the tools each profile may run: what the
// allow and approve bindings grant there, through their profile or the default one for
// the tool's risk. Denials are left out, since they grant nothing; conditions and
// narrower scopes are ignored, since a profile serves every call that reaches it. Global
// tools use no profile. opsward rbac render reads this (docs/design/0009).
func (p *Policy) Grants(cat Catalog) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for env := range cat.Environments {
		for i := range p.bindings {
			b := &p.bindings[i]
			if b.Effect == core.EffectDeny || !anyGlob(b.Scope.Environments, env) {
				continue
			}
			for name, t := range cat.Tools {
				if t.Global || !b.role.contains(name, t.Risk) {
					continue
				}
				profile := b.Profile
				if profile == "" {
					profile = core.ProfileOperator
					if t.Risk == core.RiskRead {
						profile = core.ProfileReader
					}
				}
				if out[env] == nil {
					out[env] = map[string][]string{}
				}
				if !slices.Contains(out[env][profile], name) {
					out[env][profile] = append(out[env][profile], name)
				}
			}
		}
		for _, tools := range out[env] {
			slices.Sort(tools)
		}
	}
	return out
}
