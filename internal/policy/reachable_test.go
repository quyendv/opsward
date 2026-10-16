package policy

import (
	"strings"
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
			if got := len(p.ReachableProfiles(request("alice@example.com", tt.groups, "get_workload_status", core.RiskRead, tt.scope))) > 0; got != tt.want {
				t.Fatalf("Reachable = %v, want %v", got, tt.want)
			}
		})
	}
}

// Listings read with the reader profile: a binding that grants access only through a
// narrower profile does not reach, so its cluster boundary is not bypassed. A deny or an
// approval over every namespace, or over a whole namespace, hides it.
func TestReachableRespectsProfilesAndWholeRestrictions(t *testing.T) {
	roles := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	cat := testCatalog
	for name, tt := range map[string]struct {
		bindings []Binding
		scope    core.Scope
		want     bool
	}{
		"reader profile": {[]Binding{{Name: "a", Subjects: Subjects{Groups: teamA}, Role: "viewer", Profile: "reader"}}, core.Scope{Environment: "staging"}, true},
		"deny every namespace": {[]Binding{
			{Name: "a", Subjects: Subjects{Groups: teamA}, Role: "viewer"},
			{Name: "d", Role: "viewer", Effect: core.EffectDeny, Scope: ScopeSelector{Namespaces: []string{"*"}}},
		}, core.Scope{Environment: "staging"}, false},
		"approval over a namespace": {[]Binding{
			{Name: "a", Subjects: Subjects{Groups: teamA}, Role: "viewer"},
			{Name: "ask", Role: "viewer", Effect: core.EffectApprove, Scope: ScopeSelector{Namespaces: []string{"payments"}}, Approvers: Subjects{Groups: []string{"platform"}}},
		}, core.Scope{Environment: "staging", Namespace: "payments"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, err := Compile(Config{Roles: roles, Bindings: tt.bindings}, cat, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(p.ReachableProfiles(request("alice@example.com", teamA, "get_workload_status", core.RiskRead, tt.scope))) > 0; got != tt.want {
				t.Fatalf("Reachable = %v, want %v", got, tt.want)
			}
		})
	}
}

// A listing runs once per profile the principal reaches through, so a team bound through
// its own profile lists with it.
func TestReachableProfiles(t *testing.T) {
	roles := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	p := mustCompile(t, Config{Roles: roles, Bindings: []Binding{
		{Name: "own", Subjects: Subjects{Groups: teamA}, Role: "viewer", Profile: "team-a-operator", Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"team-a-*"}}},
		{Name: "shared", Subjects: Subjects{Groups: teamA}, Role: "viewer", Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"shared"}}},
	}})
	got := p.ReachableProfiles(request("alice@example.com", teamA, "get_workload_status", core.RiskRead, core.Scope{Environment: "staging"}))
	if strings.Join(got, ",") != "reader,team-a-operator" {
		t.Fatalf("profiles = %v", got)
	}
	got = p.ReachableProfiles(request("alice@example.com", teamA, "get_workload_status", core.RiskRead, core.Scope{Environment: "staging", Namespace: "team-a-api"}))
	if strings.Join(got, ",") != "team-a-operator" {
		t.Fatalf("profiles in team-a-api = %v", got)
	}
}
