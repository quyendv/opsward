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
			"status conditions, and each pod's phase, readiness, restart count and waiting reason. " +
			"Start here when a workload is unhealthy.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			ref := capability.WorkloadRef{Namespace: v.Namespace, Kind: v.Kind, Name: v.Name}

			// The object read to resolve labels is reused only when the call runs with
			// the same profile; any other profile reads it again under its own RBAC.
			var cached *capability.Workload
			get := func(ctx context.Context, profile string) (*capability.Workload, error) {
				w, err := src.Workloads(v.Environment, profile)
				if err != nil {
					return nil, err
				}
				return w.GetWorkload(ctx, ref)
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace, Kind: v.Kind, Name: v.Name},
				Args:  obj,
				Resolve: func(ctx context.Context) (map[string]string, error) {
					w, err := get(ctx, engine.ProfileReader)
					if err != nil {
						return nil, err
					}
					cached = w
					return w.Labels, nil
				},
				Run: func(ctx context.Context, profile string) (any, error) {
					if cached != nil && profile == engine.ProfileReader {
						return cached, nil
					}
					return get(ctx, profile)
				},
			}, nil
		},
	}, nil
}
