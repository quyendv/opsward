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

// A tool whose calls may run at a lower risk (propose_change under suggest) is granted at
// each risk it can have, through the profile that risk defaults to.
func TestGrantsCountLowerRisks(t *testing.T) {
	cat := Catalog{
		Tools: map[string]ToolInfo{
			"propose_change": {Risk: core.RiskWrite, Risks: []core.Risk{core.RiskRead}, Args: []string{"environment"}},
		},
		Environments: map[string][]string{"staging": {"reader", "operator"}, "production": {"reader", "operator"}},
	}
	p, _, err := Compile(Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}, "proposer": {Tools: []string{"propose_change"}}},
		Bindings: []Binding{
			{Name: "view", Role: "viewer", Scope: ScopeSelector{Environments: []string{"staging"}}},
			{Name: "propose", Role: "proposer", Scope: ScopeSelector{Environments: []string{"production"}}},
		},
	}, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string][]string{
		"staging":    {"reader": {"propose_change"}},
		"production": {"reader": {"propose_change"}, "operator": {"propose_change"}},
	}
	if got := p.Grants(cat); !reflect.DeepEqual(got, want) {
		t.Fatalf("Grants = %v\nwant %v", got, want)
	}
}
