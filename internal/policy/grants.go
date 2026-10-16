package policy

import (
	"slices"

	"github.com/quyendv/opsward/internal/core"
)

// Grants returns, per environment of cat, the tools each profile may run: what the
// allow and approve bindings grant there, through their profile or the default one for
// each risk the tool can have. A denial that covers every caller in a whole environment
// takes its tools out there; other conditions and narrower scopes are ignored, since a
// profile serves every call that reaches it. Global tools use no profile. opsward rbac
// render reads this (docs/design/0009).
func (p *Policy) Grants(cat Catalog) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for env := range cat.Environments {
		for i := range p.bindings {
			b := &p.bindings[i]
			if b.Effect == core.EffectDeny || !anyGlob(b.Scope.Environments, env) {
				continue
			}
			for name, t := range cat.Tools {
				for _, profile := range profilesFor(b.role, name, t, b.Profile) {
					if out[env] == nil {
						out[env] = map[string][]string{}
					}
					if !slices.Contains(out[env][profile], name) {
						out[env][profile] = append(out[env][profile], name)
					}
				}
			}
		}
		for i := range p.bindings {
			if b := &p.bindings[i]; whole(b) && anyGlob(b.Scope.Environments, env) {
				for profile, tools := range out[env] {
					tools = slices.DeleteFunc(tools, func(name string) bool {
						return len(profilesFor(b.role, name, cat.Tools[name], "")) > 0
					})
					if len(tools) == 0 {
						delete(out[env], profile)
					} else {
						out[env][profile] = tools
					}
				}
			}
		}
		if len(out[env]) == 0 {
			delete(out, env)
		}
		for _, tools := range out[env] {
			slices.Sort(tools)
		}
	}
	return out
}

// whole reports whether b denies its role to every caller everywhere in the environments
// it selects: no subjects, condition or narrower scope leave a call it does not cover.
func whole(b *compiled) bool {
	s := b.Scope
	return b.Effect == core.EffectDeny && len(b.Subjects.Users) == 0 && len(b.Subjects.Groups) == 0 &&
		b.When == "" && len(s.Namespaces) == 0 && len(s.Workloads) == 0 && len(s.Apps) == 0
}
