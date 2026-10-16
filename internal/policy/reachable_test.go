package policy

import (
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

// Reachable answers whether listing within a scope can show anything: an allow binding
// whose environment and namespace patterns cover it, whatever narrower selectors it has.
func TestReachable(t *testing.T) {
	roles := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}, "other": {Tools: []string{"restart_workload"}}}
	p := mustCompile(t, Config{Roles: roles, Bindings: []Binding{
		{
			Name: "team-a-api-only", Subjects: Subjects{Groups: teamA}, Role: "viewer",
			Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"team-a-*"}, Workloads: []string{"api"}},
		},
		{
			Name: "team-a-no-secret", Subjects: Subjects{Groups: teamA}, Role: "viewer", Effect: core.EffectDeny,
			Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"team-a-secret"}},
		},
		{
			Name: "team-a-maybe", Subjects: Subjects{Groups: teamA}, Role: "viewer", Effect: core.EffectDeny,
			Scope: ScopeSelector{Namespaces: []string{"team-a-api"}}, When: `now.getHours("UTC") > 25`,
		},
		{Name: "team-b-other", Subjects: Subjects{Groups: []string{"team-b"}}, Role: "other"},
	}})
	for name, tt := range map[string]struct {
		groups []string
		scope  core.Scope
		want   bool
	}{
		"environment holding an allowed workload": {teamA, core.Scope{Environment: "staging"}, true},
		"namespace holding an allowed workload":   {teamA, core.Scope{Environment: "staging", Namespace: "team-a-api"}, true},
		"namespace outside the patterns":          {teamA, core.Scope{Environment: "staging", Namespace: "kube-system"}, false},
		"another environment":                     {teamA, core.Scope{Environment: "production"}, false},
		"namespace denied outright":               {teamA, core.Scope{Environment: "staging", Namespace: "team-a-secret"}, false},
		"deny only under a condition":             {teamA, core.Scope{Environment: "staging", Namespace: "team-a-api"}, true},
		"role without this tool":                  {[]string{"team-b"}, core.Scope{Environment: "staging"}, false},
		"no binding":                              {[]string{"nobody"}, core.Scope{Environment: "staging"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.Reachable(request("alice@example.com", tt.groups, "get_workload_status", core.RiskRead, tt.scope)); got != tt.want {
				t.Fatalf("Reachable = %v, want %v", got, tt.want)
			}
		})
	}
}
