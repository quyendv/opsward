package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/core"
)

var testCatalog = Catalog{
	Tools: map[string]ToolInfo{
		"get_workload_status": {Risk: core.RiskRead, Args: []string{"environment", "namespace", "kind", "name"}},
		"restart_workload":    {Risk: core.RiskWrite, Args: []string{"environment", "namespace", "kind", "name"}},
		"scale_workload":      {Risk: core.RiskWrite, Args: []string{"environment", "namespace", "kind", "name", "replicas"}},
		"delete_workload":     {Risk: core.RiskDestructive, Args: []string{"environment", "namespace", "kind", "name"}},
	},
	Environments: map[string][]string{
		"staging":    {"reader", "operator", "team-a-operator"},
		"production": {"reader", "operator"},
	},
}

func mustCompile(t *testing.T, cfg Config) *Policy {
	t.Helper()
	p, _, err := Compile(cfg, testCatalog, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

func request(subject string, groups []string, tool string, risk core.Risk, scope core.Scope) core.Request {
	return core.Request{
		Principal: core.Principal{Subject: subject, Groups: groups, Adapter: "test"},
		Tool:      tool, Risk: risk, Scope: scope,
		Now: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), // a Wednesday
	}
}

var (
	teamA    = []string{"team-a"}
	apiScope = core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}
)

func TestDecideDefaultDeny(t *testing.T) {
	p := mustCompile(t, Config{})
	got := p.Decide(request("alice@example.com", teamA, "get_workload_status", core.RiskRead, apiScope))
	if got.Effect != core.EffectDeny || got.Rule != core.DefaultDenyRule {
		t.Fatalf("got %+v, want default deny", got)
	}
}

func TestDecideMatchesSubjectsRoleAndScope(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{{
			Name: "team-a-view", Subjects: Subjects{Groups: []string{"team-a"}}, Role: "viewer",
			Scope: ScopeSelector{Environments: []string{"staging"}, Namespaces: []string{"team-a-*"}},
		}},
	})
	tests := []struct {
		name   string
		req    core.Request
		effect core.Effect
	}{
		{"granted", request("alice@example.com", teamA, "get_workload_status", core.RiskRead, apiScope), core.EffectAllow},
		{"other group", request("bob@example.com", []string{"team-b"}, "get_workload_status", core.RiskRead, apiScope), core.EffectDeny},
		{"risk not in role", request("alice@example.com", teamA, "restart_workload", core.RiskWrite, apiScope), core.EffectDeny},
		{"namespace outside glob", request("alice@example.com", teamA, "get_workload_status", core.RiskRead,
			core.Scope{Environment: "staging", Namespace: "team-b-api", Kind: "Deployment", Name: "api"}), core.EffectDeny},
		{"other environment", request("alice@example.com", teamA, "get_workload_status", core.RiskRead,
			core.Scope{Environment: "production", Namespace: "team-a-api", Kind: "Deployment", Name: "api"}), core.EffectDeny},
		{"no subject", request("", teamA, "get_workload_status", core.RiskRead, apiScope), core.EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Decide(tt.req); got.Effect != tt.effect {
				t.Fatalf("got %+v, want %s", got, tt.effect)
			}
		})
	}
}

func TestDecideRoleByToolName(t *testing.T) {
	p := mustCompile(t, Config{
		Roles:    map[string]Role{"restarter": {Tools: []string{"restart_workload"}}},
		Bindings: []Binding{{Name: "b", Subjects: Subjects{Users: []string{"alice@example.com"}}, Role: "restarter"}},
	})
	if got := p.Decide(request("alice@example.com", nil, "restart_workload", core.RiskWrite, apiScope)); got.Effect != core.EffectAllow {
		t.Fatalf("got %+v, want allow", got)
	}
	if got := p.Decide(request("alice@example.com", nil, "scale_workload", core.RiskWrite, apiScope)); got.Effect != core.EffectDeny {
		t.Fatalf("got %+v, want deny for a tool not in the role", got)
	}
}

func TestDecidePrecedenceDenyOverApproveOverAllow(t *testing.T) {
	roles := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	allow := Binding{Name: "allow", Role: "viewer"}
	approve := Binding{Name: "approve", Role: "viewer", Effect: core.EffectApprove, Approvers: Subjects{Groups: []string{"platform"}}}
	deny := Binding{Name: "deny", Role: "viewer", Effect: core.EffectDeny}
	req := request("alice@example.com", teamA, "get_workload_status", core.RiskRead, apiScope)

	if got := mustCompile(t, Config{Roles: roles, Bindings: []Binding{allow, approve}}).Decide(req); got.Rule != "approve" {
		t.Fatalf("allow+approve: got %+v", got)
	}
	if got := mustCompile(t, Config{Roles: roles, Bindings: []Binding{deny, allow, approve}}).Decide(req); got.Rule != "deny" {
		t.Fatalf("deny+allow+approve: got %+v", got)
	}
}

func TestDecideWorkloadAndAppScope(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{
			{Name: "api-only", Role: "viewer", Subjects: Subjects{Groups: []string{"team-a"}}, Scope: ScopeSelector{Workloads: []string{"api"}}},
			{Name: "checkout-app", Role: "viewer", Subjects: Subjects{Groups: []string{"team-c"}}, Scope: ScopeSelector{Apps: []string{"checkout"}}},
		},
	})
	worker := core.Scope{Environment: "staging", Namespace: "team-a-api", Kind: "Deployment", Name: "worker"}
	noTarget := core.Scope{Environment: "staging", Namespace: "team-a-api"}
	labelled := core.Scope{
		Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "cart",
		Labels: map[string]string{DefaultAppLabel: "checkout"}, LabelsKnown: true,
	}
	unlabelled := apiScope
	unlabelled.LabelsKnown = true
	tests := []struct {
		name   string
		req    core.Request
		effect core.Effect
	}{
		{"named workload", request("a@example.com", teamA, "get_workload_status", core.RiskRead, apiScope), core.EffectAllow},
		{"other workload", request("a@example.com", teamA, "get_workload_status", core.RiskRead, worker), core.EffectDeny},
		{"call without a target never matches a workload binding", request("a@example.com", teamA, "get_workload_status", core.RiskRead, noTarget), core.EffectDeny},
		{"app label matches", request("c@example.com", []string{"team-c"}, "get_workload_status", core.RiskRead, labelled), core.EffectAllow},
		{"app label missing", request("c@example.com", []string{"team-c"}, "get_workload_status", core.RiskRead, unlabelled), core.EffectDeny},
		{"labels unknown", request("c@example.com", []string{"team-c"}, "get_workload_status", core.RiskRead, core.Scope{
			Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "cart",
			Labels: map[string]string{DefaultAppLabel: "checkout"},
		}), core.EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Decide(tt.req); got.Effect != tt.effect {
				t.Fatalf("got %+v, want %s", got, tt.effect)
			}
		})
	}
}

func TestDecideReturnsBindingProfile(t *testing.T) {
	p := mustCompile(t, Config{
		Roles:    map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{{Name: "b", Role: "viewer", Profile: "team-a-operator", Scope: ScopeSelector{Environments: []string{"staging"}}}},
	})
	if got := p.Decide(request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope)); got.Profile != "team-a-operator" {
		t.Fatalf("got %+v, want profile team-a-operator", got)
	}
}

func TestCompileRejects(t *testing.T) {
	viewer := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unknown role", Config{Bindings: []Binding{{Name: "b", Role: "nope"}}}, `unknown role "nope"`},
		{"missing name", Config{Roles: viewer, Bindings: []Binding{{Role: "viewer"}}}, "name is required"},
		{"duplicate name", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer"}, {Name: "b", Role: "viewer"}}}, "duplicate name"},
		{"unknown effect", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", Effect: "maybe"}}}, `unknown effect "maybe"`},
		{"unknown risk", Config{Roles: map[string]Role{"r": {Risk: []core.Risk{"spicy"}}}}, `unknown risk "spicy"`},
		{"allow destructive by risk", Config{
			Roles:    map[string]Role{"admin": {Risk: []core.Risk{core.RiskDestructive}}},
			Bindings: []Binding{{Name: "b", Role: "admin"}},
		}, "never be allowed without approval"},
		{"allow destructive by tool", Config{
			Roles:    map[string]Role{"deleter": {Tools: []string{"delete_workload"}}},
			Bindings: []Binding{{Name: "b", Role: "deleter"}},
		}, "never be allowed without approval"},
		{"approve without approvers", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", Effect: core.EffectApprove}}}, "needs approvers"},
		{"unknown environment", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", Scope: ScopeSelector{Environments: []string{"prod"}}}}}, `unknown environment "prod"`},
		{"bad pattern", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", Scope: ScopeSelector{Namespaces: []string{"team-["}}}}}, "invalid pattern"},
		{"profile not in environment", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", Profile: "team-a-operator"}}}, `not declared for environment "production"`},
		{"unavailable source", Config{Sources: []string{"database"}}, `"database" is not available`},
		{"when does not compile", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", When: "principal.subject =="}}}, "when:"},
		{"when is not bool", Config{Roles: viewer, Bindings: []Binding{{Name: "b", Role: "viewer", When: "principal.subject"}}}, "must evaluate to bool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Compile(tt.cfg, testCatalog, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestCompileAllowsDestructiveBehindApproval(t *testing.T) {
	_, _, err := Compile(Config{
		Roles: map[string]Role{"admin": {Risk: []core.Risk{core.RiskDestructive}}},
		Bindings: []Binding{{
			Name: "b", Role: "admin", Effect: core.EffectApprove, Approvers: Subjects{Groups: []string{"platform"}},
		}},
	}, testCatalog, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
}

func TestCompileWarnsWhenNothingIsGranted(t *testing.T) {
	_, warnings, err := Compile(Config{}, testCatalog, nil)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "every call will be denied") {
		t.Fatalf("warnings = %v, err = %v", warnings, err)
	}
}

func TestCompileReportsEveryError(t *testing.T) {
	_, _, err := Compile(Config{Bindings: []Binding{{Name: "a", Role: "x"}, {Name: "b", Role: "y"}}}, testCatalog, nil)
	if err == nil || !strings.Contains(err.Error(), `"x"`) || !strings.Contains(err.Error(), `"y"`) {
		t.Fatalf("err = %v, want both bindings reported", err)
	}
}

// When labels could not be read, an app-scoped binding with a stricter effect than
// allow still applies, so a lookup failure cannot lift a deny or an approval.
func TestDecideUnknownLabelsFailClosed(t *testing.T) {
	viewer := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	for _, effect := range []core.Effect{core.EffectDeny, core.EffectApprove} {
		t.Run(string(effect), func(t *testing.T) {
			p := mustCompile(t, Config{Roles: viewer, Bindings: []Binding{
				{Name: "all", Role: "viewer"},
				{
					Name: "payments", Role: "viewer", Effect: effect, Approvers: Subjects{Groups: []string{"platform"}},
					Scope: ScopeSelector{Apps: []string{"payments"}},
				},
			}})
			unknown := request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope)
			if got := p.Decide(unknown); got.Rule != "payments" {
				t.Fatalf("labels unknown: got %+v, want the %s binding to apply", got, effect)
			}
			known := unknown
			known.Scope.LabelsKnown = true
			known.Scope.Labels = map[string]string{DefaultAppLabel: "shop"}
			if got := p.Decide(known); got.Rule != "all" {
				t.Fatalf("labels known and not payments: got %+v, want allow", got)
			}
		})
	}
}

// A binding without a profile runs read tools as "reader" and others as "operator";
// a config whose environment lacks them would validate and then fail on every call.
func TestCompileRequiresDefaultProfiles(t *testing.T) {
	cat := Catalog{Tools: testCatalog.Tools, Environments: map[string][]string{"dev": {"viewer"}}}
	roles := map[string]Role{
		"viewer":    {Risk: []core.Risk{core.RiskRead}},
		"restarter": {Tools: []string{"restart_workload"}},
	}
	_, _, err := Compile(Config{Roles: roles, Bindings: []Binding{{Name: "b", Role: "viewer"}}}, cat, nil)
	if err == nil || !strings.Contains(err.Error(), `needs profile "reader" in environment "dev"`) {
		t.Fatalf("err = %v", err)
	}
	_, _, err = Compile(Config{Roles: roles, Bindings: []Binding{{Name: "b", Role: "restarter"}}}, cat, nil)
	if err == nil || !strings.Contains(err.Error(), `needs profile "operator" in environment "dev"`) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := Compile(Config{Roles: roles, Bindings: []Binding{{Name: "b", Role: "viewer", Profile: "viewer"}}}, cat, nil); err != nil {
		t.Fatalf("an explicit profile should satisfy the check: %v", err)
	}
}

func TestCompileWarnsWhenAppsCannotBeResolved(t *testing.T) {
	cat := Catalog{Tools: testCatalog.Tools, Environments: map[string][]string{"dev": {"viewer"}}}
	_, warnings, err := Compile(Config{
		Roles:    map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{{Name: "b", Role: "viewer", Profile: "viewer", Scope: ScopeSelector{Apps: []string{"shop"}}}},
	}, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), `labels are read with the "reader" profile`) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestNeedsLabels(t *testing.T) {
	viewer := map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}}
	for name, tt := range map[string]struct {
		b    Binding
		want bool
	}{
		"plain":           {Binding{Name: "b", Role: "viewer"}, false},
		"apps":            {Binding{Name: "b", Role: "viewer", Scope: ScopeSelector{Apps: []string{"shop"}}}, true},
		"labels in when":  {Binding{Name: "b", Role: "viewer", When: `scope.labels["tier"] == "web"`}, true},
		"other condition": {Binding{Name: "b", Role: "viewer", When: `principal.subject == "a"`}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mustCompile(t, Config{Roles: viewer, Bindings: []Binding{tt.b}}).NeedsLabels(); got != tt.want {
				t.Fatalf("NeedsLabels = %v, want %v", got, tt.want)
			}
		})
	}
}
