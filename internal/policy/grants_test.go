package policy

import (
	"reflect"
	"strings"
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

// A binding without a profile runs a call at each risk the tool can have through that
// risk's default profile; every one must exist.
func TestCompileChecksTheDefaultProfileOfLowerRisks(t *testing.T) {
	cat := Catalog{
		Tools:        map[string]ToolInfo{"propose_change": {Risk: core.RiskWrite, Risks: []core.Risk{core.RiskRead}}},
		Environments: map[string][]string{"staging": {"operator"}},
	}
	_, _, err := Compile(Config{
		Roles:    map[string]Role{"proposer": {Tools: []string{"propose_change"}}},
		Bindings: []Binding{{Name: "propose", Role: "proposer"}},
	}, cat, nil)
	if err == nil || !strings.Contains(err.Error(), `needs profile "reader"`) {
		t.Fatalf("err = %v", err)
	}
}

// A denial that applies to every caller, everywhere in an environment, takes its tools
// out of what is granted there; a narrower one does not, since it leaves calls it does
// not cover allowed.
func TestGrantsSubtractWholeDenials(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{
			"viewer":    {Risk: []core.Risk{core.RiskRead}},
			"restarter": {Tools: []string{"restart_workload"}},
		},
		Bindings: []Binding{
			{Name: "view", Role: "viewer"},
			{Name: "restart", Role: "restarter"},
			{Name: "no-restart-in-production", Role: "restarter", Effect: core.EffectDeny, Scope: ScopeSelector{Environments: []string{"production"}}},
			{Name: "no-view-in-kube-system", Role: "viewer", Effect: core.EffectDeny, Scope: ScopeSelector{Namespaces: []string{"kube-system"}}},
			{Name: "no-view-for-bob", Role: "viewer", Effect: core.EffectDeny, Subjects: Subjects{Users: []string{"bob"}}},
		},
	})
	got := p.Grants(testCatalog)
	if _, ok := got["production"]["operator"]; ok {
		t.Errorf("production still grants operator: %v", got["production"])
	}
	if !reflect.DeepEqual(got["staging"]["operator"], []string{"restart_workload"}) || !reflect.DeepEqual(got["production"]["reader"], []string{"get_workload_status"}) {
		t.Errorf("Grants = %v", got)
	}
}

// Grantees names who reaches each profile, for a profile that acts as its caller
// (docs/design/0014).
func TestGrantees(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{
			"viewer":    {Risk: []core.Risk{core.RiskRead}},
			"restarter": {Tools: []string{"restart_workload"}},
			"scaler":    {Tools: []string{"scale_workload"}},
		},
		Bindings: []Binding{
			{Name: "alice-view", Role: "viewer", Subjects: Subjects{Users: []string{"Alice@Example.com"}}, Scope: ScopeSelector{Environments: []string{"staging"}}},
			{Name: "team-a-view", Role: "viewer", Subjects: Subjects{Groups: []string{"team-a"}}, Scope: ScopeSelector{Environments: []string{"staging"}}},
			{Name: "team-a-restart", Role: "restarter", Subjects: Subjects{Groups: []string{"team-a"}, Users: []string{"bob"}}, Profile: "team-a-operator", Scope: ScopeSelector{Environments: []string{"staging"}}},
			{Name: "prod-scale", Role: "scaler", Effect: core.EffectApprove, Approvers: Subjects{Groups: []string{"platform"}}, Scope: ScopeSelector{Environments: []string{"prod*"}}},
			// Denials and approvers reach no profile.
			{Name: "no-carol", Role: "viewer", Effect: core.EffectDeny, Subjects: Subjects{Users: []string{"carol"}}},
		},
	})
	want := map[string]map[string]Grantees{
		"staging": {
			"reader":          {Users: []string{"alice@example.com"}, Groups: []string{"team-a"}},
			"team-a-operator": {Users: []string{"bob"}, Groups: []string{"team-a"}},
		},
		"production": {
			"operator": {Anyone: true},
		},
	}
	if got := p.Grantees(testCatalog); !reflect.DeepEqual(got, want) {
		t.Fatalf("Grantees = %+v\nwant %+v", got, want)
	}
}
