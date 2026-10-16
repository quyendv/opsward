package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

// EnvironmentInfo describes a configured environment to list_environments.
type EnvironmentInfo struct {
	Name       string   `json:"name"`
	Cluster    string   `json:"cluster"`
	Namespaces []string `json:"namespaces"` // patterns
	Metrics    bool     `json:"metrics"`
	Logs       bool     `json:"logs"`
}

// Environments hands out the configured environments; the answer changes on reload.
type Environments interface {
	Environments() []EnvironmentInfo
}

func environment(envs Environments, name string) (EnvironmentInfo, bool) {
	for _, e := range envs.Environments() {
		if e.Name == name {
			return e, true
		}
	}
	return EnvironmentInfo{}, false
}

// ListEnvironments is list_environments: the environments the caller can work in.
func ListEnvironments(envs Environments) (engine.Tool, error) {
	in, err := newInput[struct{}](nil)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "list_environments",
		Description: "List the environments you can work in: name, cluster, namespace patterns, and whether metrics " +
			"and logs are available. Start here when you do not know an environment's name.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names, Global: true, List: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			_, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{Args: obj, RunList: func(context.Context, string) ([]engine.Item, error) {
				var items []engine.Item
				for _, e := range envs.Environments() {
					items = append(items, engine.Item{Scope: core.Scope{Environment: e.Name}, Data: e})
				}
				return items, nil
			}}, nil
		},
	}, nil
}

type environmentInput struct {
	Environment string `json:"environment" jsonschema:"name of a configured environment"`
}

// ListNamespaces is list_namespaces: the environment's namespaces the caller can work in.
func ListNamespaces(envs Environments, ws capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[environmentInput](func(s *jsonschema.Schema) {
		s.Properties["environment"].MinLength = jsonschema.Ptr(1)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name:        "list_namespaces",
		Description: "List the namespaces of an environment that you can work in.",
		Risk:        core.RiskRead, InputSchema: in.schema, Args: in.names, List: true,
		Needs: []string{capability.OpNamespacesList},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment}, Args: obj,
				RunList: func(ctx context.Context, profile string) ([]engine.Item, error) {
					env, ok := environment(envs, v.Environment)
					if !ok {
						return nil, fmt.Errorf("unknown environment %q", v.Environment)
					}
					w, err := ws.Workloads(v.Environment, profile)
					if err != nil {
						return nil, err
					}
					names, err := w.ListNamespaces(ctx)
					if err != nil {
						return nil, err
					}
					var items []engine.Item
					for _, n := range names {
						if inPatterns(env.Namespaces, n) {
							items = append(items, engine.Item{Scope: core.Scope{Environment: v.Environment, Namespace: n}, Data: n})
						}
					}
					return items, nil
				},
			}, nil
		},
	}, nil
}

func inPatterns(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

type listWorkloadsInput struct {
	Environment string `json:"environment" jsonschema:"name of a configured environment"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"namespace to list; omit for every namespace of the environment you can work in"`
	Unhealthy   bool   `json:"unhealthy,omitempty" jsonschema:"only workloads with fewer ready replicas than desired"`
}

// ListWorkloads is list_workloads: the workloads the caller can work on, in a namespace
// or across the environment.
func ListWorkloads(envs Environments, ws capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[listWorkloadsInput](func(s *jsonschema.Schema) {
		s.Properties["environment"].MinLength = jsonschema.Ptr(1)
		s.Properties["namespace"].MinLength = jsonschema.Ptr(1)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "list_workloads",
		Description: "List the Deployments, StatefulSets and DaemonSets you can work on, with desired and ready " +
			"replicas: in one namespace, or across the environment without one. unhealthy keeps only those with " +
			"fewer ready replicas than desired, the quickest way to find what is broken.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names, List: true,
		Needs: []string{capability.OpWorkloadsList},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace}, Args: obj,
				RunList: func(ctx context.Context, profile string) ([]engine.Item, error) {
					env, ok := environment(envs, v.Environment)
					if !ok {
						return nil, fmt.Errorf("unknown environment %q", v.Environment)
					}
					w, err := ws.Workloads(v.Environment, profile)
					if err != nil {
						return nil, err
					}
					list, err := listWorkloads(ctx, w, env, v.Namespace)
					if err != nil {
						return nil, err
					}
					items := make([]engine.Item, 0, len(list))
					for _, s := range list {
						// A named namespace was checked against the environment before the call ran.
						if (v.Namespace == "" && !inPatterns(env.Namespaces, s.Namespace)) || (v.Unhealthy && s.Ready >= s.Desired) {
							continue
						}
						items = append(items, engine.Item{
							Scope: core.Scope{
								Environment: v.Environment, Namespace: s.Namespace, Kind: s.Kind, Name: s.Name,
								Labels: s.Labels, LabelsKnown: true,
							},
							Data: s,
						})
					}
					return items, nil
				},
			}, nil
		},
	}, nil
}

// listWorkloads lists one namespace, or the environment: an environment spanning every
// namespace in one request, otherwise namespace by namespace, so that a reader bound only
// in the environment's namespaces can list it.
func listWorkloads(ctx context.Context, w capability.Workloads, env EnvironmentInfo, namespace string) ([]capability.WorkloadSummary, error) {
	if namespace != "" || slices.Contains(env.Namespaces, "*") {
		return w.ListWorkloads(ctx, namespace)
	}
	names, err := w.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var out []capability.WorkloadSummary
	for _, n := range names {
		if !inPatterns(env.Namespaces, n) {
			continue
		}
		list, err := w.ListWorkloads(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}
