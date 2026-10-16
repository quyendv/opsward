package policy

import (
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

// A call without a namespace covers every namespace. A deny or approval on any of them,
// whatever its narrower selectors or condition, must still apply; an allow must cover
// every namespace and must not depend on the target in its condition.
func TestClusterWideCallsKeepNarrowerRestrictions(t *testing.T) {
	roles := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	allow := Binding{Name: "team-a-all", Subjects: Subjects{Groups: teamA}, Role: "viewer", Scope: ScopeSelector{Environments: []string{"staging"}}}
	wide := core.Scope{Environment: "staging"}
	for name, tt := range map[string]struct {
		bindings []Binding
		want     string
	}{
		"namespace deny":     {[]Binding{allow, {Name: "no-kube-system", Role: "viewer", Effect: core.EffectDeny, Scope: ScopeSelector{Namespaces: []string{"kube-system"}}}}, "no-kube-system"},
		"workload deny":      {[]Binding{allow, {Name: "no-db", Role: "viewer", Effect: core.EffectDeny, Scope: ScopeSelector{Workloads: []string{"db"}}}}, "no-db"},
		"conditional deny":   {[]Binding{allow, {Name: "freeze", Role: "viewer", Effect: core.EffectDeny, When: `scope.namespace == "kube-system"`}}, "freeze"},
		"namespace approval": {[]Binding{allow, {Name: "ask", Role: "viewer", Effect: core.EffectApprove, Scope: ScopeSelector{Namespaces: []string{"payments"}}, Approvers: Subjects{Groups: []string{"platform"}}}}, "ask"},
		"allow on target":    {[]Binding{{Name: "not-kube-system", Subjects: Subjects{Groups: teamA}, Role: "viewer", When: `scope.namespace != "kube-system"`}}, core.DefaultDenyRule},
		"allow on some ns":   {[]Binding{{Name: "team-ns", Subjects: Subjects{Groups: teamA}, Role: "viewer", Scope: ScopeSelector{Namespaces: []string{"team-a-*"}}}}, core.DefaultDenyRule},
		"allow on every ns":  {[]Binding{{Name: "star", Subjects: Subjects{Groups: teamA}, Role: "viewer", Scope: ScopeSelector{Namespaces: []string{"*"}}}}, "star"},
		"allow in time":      {[]Binding{{Name: "hours", Subjects: Subjects{Groups: teamA}, Role: "viewer", When: `now.getHours("UTC") >= 0`}}, "hours"},
		"deny elsewhere":     {[]Binding{allow, {Name: "prod-only", Role: "viewer", Effect: core.EffectDeny, Scope: ScopeSelector{Environments: []string{"production"}}}}, "team-a-all"},
	} {
		t.Run(name, func(t *testing.T) {
			p := mustCompile(t, Config{Roles: roles, Bindings: tt.bindings})
			if got := p.Decide(request("alice@example.com", teamA, "get_workload_status", core.RiskRead, wide)); got.Rule != tt.want {
				t.Fatalf("rule = %s (%s), want %s", got.Rule, got.Effect, tt.want)
			}
		})
	}
}
