package policy

import (
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

func TestNaming(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{
			"viewer":   {Risk: []core.Risk{core.RiskRead}},
			"operator": {Tools: []string{"restart_workload"}},
		},
		Bindings: []Binding{
			{Name: "everyone-views", Role: "viewer"},
			{
				Name: "team-a-operates", Subjects: Subjects{Groups: []string{"team-a"}}, Role: "operator",
				Scope: ScopeSelector{Namespaces: []string{"team-a-*"}}, Effect: core.EffectApprove, Approvers: Subjects{Groups: []string{"platform"}},
			},
			{Name: "bob-views", Subjects: Subjects{Users: []string{"bob@example.com"}}, Role: "viewer"},
		},
	})
	alice := core.Principal{Subject: "Alice@Example.com", Groups: []string{"team-a"}}
	grants, approves := p.Naming(alice)
	if len(grants) != 2 || grants[0].Name != "everyone-views" || grants[1].Name != "team-a-operates" || len(approves) != 0 {
		t.Fatalf("alice: grants %+v, approves %+v", grants, approves)
	}
	if grants[1].Role != "operator" || grants[1].Tools[0] != "restart_workload" || grants[1].Scope.Namespaces[0] != "team-a-*" ||
		grants[1].Effect != core.EffectApprove || grants[1].Approvers.Groups[0] != "platform" {
		t.Fatalf("binding view = %+v", grants[1])
	}
	grants, approves = p.Naming(core.Principal{Subject: "carol", Groups: []string{"platform"}})
	if len(grants) != 1 || len(approves) != 1 || approves[0].Name != "team-a-operates" {
		t.Fatalf("carol: grants %+v, approves %+v", grants, approves)
	}
}
