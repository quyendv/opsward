package policy

import (
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

// A tool granted only by name, such as audit search, is not part of a risk class: a
// viewer does not become an auditor.
func TestRoleGrantsByNameToolsOnlyByName(t *testing.T) {
	cat := Catalog{
		Tools: map[string]ToolInfo{
			"get_workload_status": {Risk: core.RiskRead},
			"search_audit":        {Risk: core.RiskRead, Global: true, ByName: true},
		},
		Environments: testCatalog.Environments,
	}
	p, _, err := Compile(Config{
		Roles: map[string]Role{
			"viewer":  {Risk: []core.Risk{core.RiskRead}},
			"auditor": {Tools: []string{"search_audit"}},
		},
		Bindings: []Binding{
			{Name: "view", Subjects: Subjects{Groups: []string{"team-a"}}, Role: "viewer"},
			{Name: "audit", Subjects: Subjects{Groups: []string{"audit"}}, Role: "auditor"},
		},
	}, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := core.Scope{Environment: "staging", Namespace: "team-a-api"}
	if got := p.Decide(request("alice", teamA, "get_workload_status", core.RiskRead, scope)); got.Effect != core.EffectAllow {
		t.Fatalf("viewer reading a workload: %+v", got)
	}
	if got := p.Decide(request("alice", teamA, "search_audit", core.RiskRead, scope)); got.Effect != core.EffectDeny {
		t.Fatalf("a risk class granted search_audit: %+v", got)
	}
	if got := p.ReachableProfiles(request("alice", teamA, "search_audit", core.RiskRead, scope)); len(got) != 0 {
		t.Fatalf("a risk class reaches search_audit: %v", got)
	}
	if got := p.Decide(request("bob", []string{"audit"}, "search_audit", core.RiskRead, scope)); got.Effect != core.EffectAllow {
		t.Fatalf("a role naming search_audit: %+v", got)
	}
}
