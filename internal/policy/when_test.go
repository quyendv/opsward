package policy

import (
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

func TestWhenConditions(t *testing.T) {
	viewer := map[string]Role{
		"viewer": {Risk: []core.Risk{core.RiskRead}},
		"scaler": {Tools: []string{"scale_workload"}},
	}
	tests := []struct {
		name   string
		b      Binding
		req    core.Request
		effect core.Effect
	}{
		{
			name:   "true condition applies",
			b:      Binding{Name: "b", Role: "viewer", When: `now.getDayOfWeek("UTC") in [1, 2, 3, 4, 5]`},
			req:    request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope),
			effect: core.EffectAllow,
		},
		{
			name:   "false condition does not apply",
			b:      Binding{Name: "b", Role: "viewer", When: `now.getDayOfWeek("UTC") in [0, 6]`},
			req:    request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope),
			effect: core.EffectDeny,
		},
		{
			name:   "condition on principal groups",
			b:      Binding{Name: "b", Role: "viewer", When: `"team-a" in principal.groups`},
			req:    request("a@example.com", teamA, "get_workload_status", core.RiskRead, apiScope),
			effect: core.EffectAllow,
		},
		{
			name:   "condition on labels",
			b:      Binding{Name: "b", Role: "viewer", When: `scope.labels["tier"] == "web"`},
			req:    request("a@example.com", nil, "get_workload_status", core.RiskRead, core.Scope{Environment: "staging", Name: "api", Labels: map[string]string{"tier": "web"}}),
			effect: core.EffectAllow,
		},
		{
			name: "condition on arguments",
			b:    Binding{Name: "b", Role: "scaler", When: `args.replicas <= 5`},
			req: func() core.Request {
				r := request("a@example.com", nil, "scale_workload", core.RiskWrite, apiScope)
				r.Args = map[string]any{"replicas": 3}
				return r
			}(),
			effect: core.EffectAllow,
		},
		{
			name:   "allow whose condition errors does not apply",
			b:      Binding{Name: "b", Role: "viewer", When: `args.replicas <= 5`},
			req:    request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope),
			effect: core.EffectDeny,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mustCompile(t, Config{Roles: viewer, Bindings: []Binding{tt.b}})
			if got := p.Decide(tt.req); got.Effect != tt.effect {
				t.Fatalf("got %+v, want %s", got, tt.effect)
			}
		})
	}
}

func TestWhenDenyThatErrorsStillDenies(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{
			{Name: "allow", Role: "viewer"},
			{Name: "freeze", Role: "viewer", Effect: core.EffectDeny, When: `args.missing == 1`},
		},
	})
	got := p.Decide(request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope))
	if got.Effect != core.EffectDeny || got.Rule != "freeze" {
		t.Fatalf("got %+v, want the erroring deny binding to apply", got)
	}
}

func TestWhenCostLimit(t *testing.T) {
	p := mustCompile(t, Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{{
			Name: "b", Role: "viewer",
			When: `[1,2,3,4,5,6,7,8,9,10].all(a, [1,2,3,4,5,6,7,8,9,10].all(b, [1,2,3,4,5,6,7,8,9,10].all(c, [1,2,3,4,5,6,7,8,9,10].all(d, [1,2,3,4,5,6,7,8,9,10].all(e, [1,2,3,4,5,6,7,8,9,10].all(f, a+b+c+d+e+f > 0))))))`,
		}},
	})
	got := p.Decide(request("a@example.com", nil, "get_workload_status", core.RiskRead, apiScope))
	if got.Effect != core.EffectDeny {
		t.Fatalf("got %+v, want deny once the cost limit stops evaluation", got)
	}
}

func TestWhenWarnings(t *testing.T) {
	_, warnings, err := Compile(Config{
		Roles: map[string]Role{"viewer": {Risk: []core.Risk{core.RiskRead}}},
		Bindings: []Binding{{
			Name: "b", Role: "viewer",
			When: `now.getHours() < 18 && args.replicas < 3`,
		}},
	}, testCatalog, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"without a time zone", `argument "replicas"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q missing %q", joined, want)
		}
	}
}
