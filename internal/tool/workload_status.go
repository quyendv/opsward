package tool

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

type workloadInput struct {
	Environment string `json:"environment" jsonschema:"name of a configured environment"`
	Namespace   string `json:"namespace" jsonschema:"namespace of the workload"`
	Kind        string `json:"kind" jsonschema:"kind of the workload"`
	Name        string `json:"name" jsonschema:"name of the workload"`
}

func refineWorkloadInput(s *jsonschema.Schema) {
	kinds := make([]any, len(capability.WorkloadKinds))
	for i, k := range capability.WorkloadKinds {
		kinds[i] = k
	}
	s.Properties["kind"].Enum = kinds
	for _, f := range []string{"environment", "namespace", "name"} {
		s.Properties[f].MinLength = jsonschema.Ptr(1)
	}
}

// WorkloadStatus is get_workload_status: replicas, conditions and pod states of one
// workload.
func WorkloadStatus(src capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[workloadInput](refineWorkloadInput)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "get_workload_status",
		Description: "Show a workload's rollout state: desired, ready, updated and available replicas, " +
			"status conditions, and each pod's phase, readiness, restart count, waiting reason and last termination " +
			"(reason such as OOMKilled, and exit code). Start here when a workload is unhealthy.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return workloadPrepared(src, v, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
				return w.GetWorkload(ctx, ref)
			}), nil
		},
	}, nil
}

// workloadPrepared builds the call for a tool that targets one workload. Before
// authorization it reads only the labels, with Opsward's reader profile; after it, run
// reads what the tool needs under the call's own profile and RBAC.
func workloadPrepared(src capability.WorkloadSource, v workloadInput, obj map[string]any,
	run func(context.Context, capability.Workloads, capability.WorkloadRef) (any, error),
) *engine.Prepared {
	ref := capability.WorkloadRef{Namespace: v.Namespace, Kind: v.Kind, Name: v.Name}
	return &engine.Prepared{
		Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace, Kind: v.Kind, Name: v.Name},
		Args:  obj,
		Resolve: func(ctx context.Context) (map[string]string, error) {
			w, err := src.Workloads(v.Environment, engine.ProfileReader)
			if err != nil {
				return nil, err
			}
			return w.WorkloadLabels(ctx, ref)
		},
		Run: func(ctx context.Context, profile string) (any, error) {
			w, err := src.Workloads(v.Environment, profile)
			if err != nil {
				return nil, err
			}
			return run(ctx, w, ref)
		},
	}
}
