package policy

import (
	"reflect"
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

func TestGrants(t *testing.T) {
	cat := testCatalog
	cat.Tools["search_knowledge"] = ToolInfo{Risk: core.RiskRead, Global: true}
	defer delete(cat.Tools, "search_knowledge")
	p := mustCompile(t, Config{
		Roles: map[string]Role{
			"viewer":    {Risk: []core.Risk{core.RiskRead}},
			"restarter": {Tools: []string{"restart_workload"}},
			"scaler":    {Tools: []string{"scale_workload"}},
		},
		Bindings: []Binding{
			{Name: "all-view", Role: "viewer"},
			{Name: "team-a-restart", Role: "restarter", Profile: "team-a-operator", Scope: ScopeSelector{Environments: []string{"staging"}}},
			{Name: "prod-scale", Role: "scaler", Effect: core.EffectApprove, Approvers: Subjects{Groups: []string{"platform"}}, Scope: ScopeSelector{Environments: []string{"prod*"}}},
			// Denials grant nothing.
			{Name: "no-staging-scale", Role: "scaler", Effect: core.EffectDeny, Scope: ScopeSelector{Environments: []string{"staging"}}},
		},
	})
	want := map[string]map[string][]string{
		"staging": {
			"reader":          {"get_workload_status"},
			"team-a-operator": {"restart_workload"},
		},
		"production": {
			"reader":   {"get_workload_status"},
			"operator": {"scale_workload"},
		},
	}
	if got := p.Grants(cat); !reflect.DeepEqual(got, want) {
		t.Fatalf("Grants = %v\nwant %v", got, want)
	}
}
