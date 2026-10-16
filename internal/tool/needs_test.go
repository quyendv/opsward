package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/source"
)

// callMethods maps fakeSource's recorded calls to the Workloads methods they stand for.
var callMethods = map[string]string{
	"get": "GetWorkload", "labels": "WorkloadLabels", "metadata": "WorkloadMetadata",
	"events": "WorkloadEvents", "logs": "WorkloadLogs", "pattern": "PodNamePattern",
	"restart": "RestartWorkload", "scale": "ScaleWorkload",
	"namespaces": "ListNamespaces", "workloads": "ListWorkloads",
}

// Each tool declares exactly the operations it performs on the workload source, so that
// the RBAC rendered from the declarations is neither short nor wide. The reader's label
// lookup before authorization is the engine's and is rendered separately.
func TestToolsDeclareWhatTheyUse(t *testing.T) {
	crashing := &capability.Workload{Labels: map[string]string{"app": "api"}, Pods: []capability.Pod{
		{
			Name: "api-a", Phase: "Running", Restarts: 3, Waiting: "CrashLoopBackOff",
			LastTerminated: &capability.Termination{Container: "api", Reason: "Error", ExitCode: 1},
		},
	}}
	envs := fixedEnvironments{
		{Name: "staging", Cluster: "cluster-a", Namespaces: []string{"*"}, Metrics: true},
		{Name: "literal", Cluster: "cluster-a", Namespaces: []string{"shop", "web"}},
		{Name: "patterns", Cluster: "cluster-a", Namespaces: []string{"sh*"}},
	}
	w := `"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"`
	tests := []struct {
		build func(*fakeSource) (engine.Tool, error)
		args  string
	}{
		{func(s *fakeSource) (engine.Tool, error) { return WorkloadStatus(s) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return WorkloadEvents(s) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return WorkloadLogs(s) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return DiagnoseWorkload(s, &fakeMetrics{}) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return GetWorkloadMetrics(s, &fakeMetrics{}) }, `{` + w + `,"metric":"cpu"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListNamespaces(envs, s) }, `{"environment":"staging"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListWorkloads(envs, s) }, `{"environment":"staging","namespace":"shop"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListWorkloads(envs, s) }, `{"environment":"staging"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListWorkloads(envs, s) }, `{"environment":"literal"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListWorkloads(envs, s) }, `{"environment":"patterns"}`},
		{func(s *fakeSource) (engine.Tool, error) { return ListNamespaces(envs, s) }, `{"environment":"literal"}`},
		{func(s *fakeSource) (engine.Tool, error) { return RestartWorkload(s) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return ScaleWorkload(s) }, `{` + w + `,"replicas":3}`},
		{func(s *fakeSource) (engine.Tool, error) {
			_, _, cfg := changeFixture("pull-request")
			return ResolveSource(s, cfg)
		}, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) {
			return DeliveryStatus(s, staticChanges{&Changes{Resolve: source.Options{ApplicationNamespace: "argocd", Applications: appWith("Synced", 0)}}})
		}, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) {
			_, _, cfg := changeFixture("pull-request")
			return ProposeChange(s, cfg)
		}, `{` + w + `,"title":"t","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 3"}]}`},
	}
	// A tool may appear with several arguments; what it declares is checked against the
	// union of what they use.
	used := map[string][]string{}
	needs := map[string][]string{}
	for _, tt := range tests {
		src := &fakeSource{
			workload: crashing, change: &capability.WorkloadChange{Diff: "d"}, logs: &capability.WorkloadLogs{},
			annotations: map[string]string{"opsward/source": "https://git.example.com/team-a/gitops.git//apps/api"},
			namespaces:  []string{"shop"}, summaries: []capability.WorkloadSummary{{Namespace: "shop", Kind: "Deployment", Name: "api"}},
		}
		tl, err := tt.build(src)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(tl.Name+tt.args, func(t *testing.T) {
			p, err := tl.Prepare(json.RawMessage(tt.args))
			if err != nil {
				t.Fatal(err)
			}
			ctx := core.WithAuditID(context.Background(), "x")
			if p.Preview != nil {
				if _, err := p.Preview(ctx, engine.ProfileOperator); err != nil {
					t.Fatal(err)
				}
			}
			if p.RunList != nil {
				_, err = p.RunList(ctx, engine.ProfileReader)
			} else {
				_, err = p.Run(ctx, engine.ProfileReader)
			}
			if err != nil {
				t.Fatal(err)
			}
			needs[tl.Name] = tl.Needs
			for _, c := range src.calls {
				name, _, _ := strings.Cut(c, ":")
				ops, ok := capability.WorkloadsOps[callMethods[name]]
				if !ok {
					t.Fatalf("call %q has no operations in capability.WorkloadsOps", c)
				}
				for _, op := range ops {
					if !slices.Contains(tl.Needs, op) {
						t.Errorf("call %q uses %s, which the tool does not declare (%v)", c, op, tl.Needs)
					}
					if !slices.Contains(used[tl.Name], op) {
						used[tl.Name] = append(used[tl.Name], op)
					}
				}
			}
		})
	}
	for name, declared := range needs {
		for _, op := range declared {
			if !slices.Contains(used[name], op) {
				t.Errorf("%s declares %s, which no call used", name, op)
			}
		}
	}
}
