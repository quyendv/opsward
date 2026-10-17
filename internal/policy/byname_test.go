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

// A deny by risk class still covers a tool granted only by name: naming is needed to
// grant, never to take away.
func TestRiskClassDenyCoversByNameTools(t *testing.T) {
	cat := Catalog{
		Tools:        map[string]ToolInfo{"search_audit": {Risk: core.RiskRead, Global: true, ByName: true}},
		Environments: testCatalog.Environments,
	}
	p, _, err := Compile(Config{
		Roles: map[string]Role{
			"auditor": {Tools: []string{"search_audit"}},
			"reads":   {Risk: []core.Risk{core.RiskRead}},
		},
		Bindings: []Binding{
			{Name: "sre-audits", Subjects: Subjects{Groups: []string{"sre"}}, Role: "auditor"},
			{
				Name: "contractors-read-nothing-in-production", Subjects: Subjects{Groups: []string{"contractors"}}, Role: "reads",
				Scope: ScopeSelector{Environments: []string{"production"}}, Effect: core.EffectDeny,
			},
		},
	}, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := core.Scope{Environment: "production", Namespace: "team-a"}
	if got := p.Decide(request("dan", []string{"sre", "contractors"}, "search_audit", core.RiskRead, scope)); got.Effect != core.EffectDeny {
		t.Fatalf("a contractor in sre: %+v", got)
	}
	if got := p.Decide(request("erin", []string{"sre"}, "search_audit", core.RiskRead, scope)); got.Effect != core.EffectAllow {
		t.Fatalf("sre alone: %+v", got)
	}
}

func TestMayAllow(t *testing.T) {
	cat := Catalog{
		Tools:        map[string]ToolInfo{"search_audit": {Risk: core.RiskRead, Global: true, ByName: true}},
		Environments: testCatalog.Environments,
	}
	p, _, err := Compile(Config{
		Roles: map[string]Role{"auditor": {Tools: []string{"search_audit"}}, "reads": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{
			{
				Name: "team-a-audits", Subjects: Subjects{Groups: []string{"team-a"}}, Role: "auditor",
				Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"team-a-*"}},
			},
			{Name: "everyone-reads", Role: "reads"},
			{Name: "bob-denied", Subjects: Subjects{Users: []string{"bob"}}, Role: "auditor", Effect: core.EffectDeny},
		},
	}, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Wherever the grant applies: the call names no scope.
	if !p.MayAllow(request("alice", teamA, "search_audit", core.RiskRead, core.Scope{})) {
		t.Fatal("team-a's auditor")
	}
	// A deny is not a grant, nor is a risk class.
	if p.MayAllow(request("bob", nil, "search_audit", core.RiskRead, core.Scope{})) {
		t.Fatal("bob")
	}
}
