// Package policy decides whether a principal may make a call: roles granted to subjects
// by bindings over a scope, with optional CEL conditions. See docs/design/0002.
package policy

import (
	"log/slog"
	"path"

	"cel.dev/cel-go/cel"

	"github.com/quyendv/opsward/internal/core"
)

// DefaultAppLabel is the label `scope.apps` matches when policy.appLabel is not set.
const DefaultAppLabel = "app.kubernetes.io/part-of"

// Config is the policy section of the configuration file.
type Config struct {
	Sources  []string        `yaml:"sources"`
	AppLabel string          `yaml:"appLabel"`
	Roles    map[string]Role `yaml:"roles"`
	Bindings []Binding       `yaml:"bindings"`
}

// Role names what may be done: a tool is in the role if its name or its risk is listed.
type Role struct {
	Risk  []core.Risk `yaml:"risk"`
	Tools []string    `yaml:"tools"`
}

// Subjects selects principals. Empty means every authenticated principal.
type Subjects struct {
	Users  []string `yaml:"users"`
	Groups []string `yaml:"groups"`
}

// ScopeSelector restricts where a binding applies. Each field is a list of glob
// patterns; an empty field matches anything.
type ScopeSelector struct {
	Environments []string `yaml:"environments"`
	Namespaces   []string `yaml:"namespaces"`
	Workloads    []string `yaml:"workloads"`
	Apps         []string `yaml:"apps"`
}

// Binding grants a role to subjects over a scope, with an effect.
type Binding struct {
	Name      string        `yaml:"name"`
	Subjects  Subjects      `yaml:"subjects"`
	Role      string        `yaml:"role"`
	Scope     ScopeSelector `yaml:"scope"`
	Effect    core.Effect   `yaml:"effect"`
	Approvers Subjects      `yaml:"approvers"`
	When      string        `yaml:"when"`
	Profile   string        `yaml:"profile"`
}

// Catalog is what policy validates against: the registered tools and the configured
// environments.
type Catalog struct {
	Tools map[string]ToolInfo
	// Environments maps an environment name to the profiles declared for it.
	Environments map[string][]string
}

// ToolInfo describes a registered tool to policy.
type ToolInfo struct {
	Risk core.Risk
	Args []string
}

// Policy is a compiled, validated policy. It is safe for concurrent use.
type Policy struct {
	appLabel string
	bindings []compiled
	log      *slog.Logger
}

type compiled struct {
	Binding
	role Role
	when cel.Program
}

// Decide authorizes a request. deny overrides approve, which overrides allow; no
// matching binding is a deny.
func (p *Policy) Decide(req core.Request) core.Decision {
	var best *compiled
	for i := range p.bindings {
		b := &p.bindings[i]
		if !b.matches(req, p.appLabel) || !p.conditionHolds(b, req) {
			continue
		}
		if best == nil || rank(b.Effect) > rank(best.Effect) {
			best = b
		}
	}
	if best == nil {
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	}
	return core.Decision{Effect: best.Effect, Rule: best.Name, Profile: best.Profile}
}

func rank(e core.Effect) int {
	switch e {
	case core.EffectDeny:
		return 2
	case core.EffectApprove:
		return 1
	case core.EffectAllow:
		return 0
	}
	return 0
}

func (b *compiled) matches(req core.Request, appLabel string) bool {
	return b.Subjects.match(req.Principal) &&
		b.role.contains(req.Tool, req.Risk) &&
		b.Scope.match(req.Scope, appLabel, b.Effect != core.EffectAllow)
}

func (s Subjects) match(p core.Principal) bool {
	if p.Subject == "" {
		return false
	}
	if len(s.Users) == 0 && len(s.Groups) == 0 {
		return true
	}
	for _, u := range s.Users {
		if u == p.Subject {
			return true
		}
	}
	for _, g := range s.Groups {
		for _, pg := range p.Groups {
			if g == pg {
				return true
			}
		}
	}
	return false
}

func (r Role) contains(tool string, risk core.Risk) bool {
	for _, t := range r.Tools {
		if t == tool {
			return true
		}
	}
	for _, rr := range r.Risk {
		if rr == risk {
			return true
		}
	}
	return false
}

// match reports whether the selector covers sc. strict is set for bindings whose effect
// is stricter than allow: when the target's labels are unknown, such a binding still
// matches on apps, so a failed lookup cannot lift a deny or an approval.
func (s ScopeSelector) match(sc core.Scope, appLabel string, strict bool) bool {
	if !anyGlob(s.Environments, sc.Environment) || !anyGlob(s.Namespaces, sc.Namespace) {
		return false
	}
	// A binding narrowed to workloads or apps never matches a call without a target.
	if (len(s.Workloads) > 0 || len(s.Apps) > 0) && sc.Name == "" {
		return false
	}
	if !anyGlob(s.Workloads, sc.Name) {
		return false
	}
	if len(s.Apps) > 0 {
		if !sc.LabelsKnown {
			return strict
		}
		v, ok := sc.Labels[appLabel]
		if !ok || !anyGlob(s.Apps, v) {
			return false
		}
	}
	return true
}

func anyGlob(patterns []string, v string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, v); ok {
			return true
		}
	}
	return false
}
