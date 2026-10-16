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
)

// callOps maps fakeSource's recorded calls to the operations they perform, as the
// Kubernetes provider implements them (docs/design/0009).
var callOps = map[string][]string{
	"get":        {capability.OpWorkloadsGet, capability.OpWorkloadsPods},
	"labels":     {capability.OpWorkloadsGet},
	"metadata":   {capability.OpWorkloadsGet},
	"events":     {capability.OpWorkloadsGet, capability.OpWorkloadsPods, capability.OpWorkloadsEvents},
	"logs":       {capability.OpWorkloadsGet, capability.OpWorkloadsPods, capability.OpWorkloadsLogs},
	"pattern":    {capability.OpWorkloadsGet, capability.OpWorkloadsPods},
	"restart":    {capability.OpWorkloadsGet, capability.OpWorkloadsRestart},
	"scale":      {capability.OpWorkloadsGet, capability.OpWorkloadsScale},
	"namespaces": {capability.OpNamespacesList},
	"workloads":  {capability.OpWorkloadsList},
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
	envs := fixedEnvironments{{Name: "staging", Cluster: "cluster-a", Namespaces: []string{"*"}, Metrics: true}}
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
		{func(s *fakeSource) (engine.Tool, error) { return RestartWorkload(s) }, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) { return ScaleWorkload(s) }, `{` + w + `,"replicas":3}`},
		{func(s *fakeSource) (engine.Tool, error) {
			_, _, cfg := changeFixture("pull-request")
			return ResolveSource(s, cfg)
		}, `{` + w + `}`},
		{func(s *fakeSource) (engine.Tool, error) {
			_, _, cfg := changeFixture("pull-request")
			return ProposeChange(s, cfg)
		}, `{` + w + `,"title":"t","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 3"}]}`},
	}
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
		t.Run(tl.Name, func(t *testing.T) {
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
			var used []string
			for _, c := range src.calls {
				name, _, _ := strings.Cut(c, ":")
				for _, op := range callOps[name] {
					if !slices.Contains(used, op) {
						used = append(used, op)
					}
				}
			}
			slices.Sort(used)
			needs := slices.Sorted(slices.Values(tl.Needs))
			if !slices.Equal(used, needs) {
				t.Fatalf("calls %v use %v; the tool declares %v", src.calls, used, needs)
			}
		})
	}
}
