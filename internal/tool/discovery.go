package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

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
		Name: "list_namespaces",
		Description: "List the namespaces of an environment that you can work in. unmatchedPatterns names configured " +
			"namespaces that do not exist in the cluster.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names, List: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			var unmatched []string
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
					for _, p := range env.Namespaces {
						// A literal pattern that names no namespace is usually a typing mistake.
						if !strings.ContainsAny(p, `*?[\`) && !slices.Contains(names, p) {
							unmatched = append(unmatched, p)
						}
					}
					return items, nil
				},
				Wrap: func(kept []any) any {
					out := map[string]any{"namespaces": kept}
					if len(unmatched) > 0 {
						out["unmatchedPatterns"] = unmatched
					}
					return out
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

// ListWorkloads is list_workloads: a namespace's workloads the caller can work on.
func ListWorkloads(ws capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[namespaceInput](refineNamespaceInput)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "list_workloads",
		Description: "List the Deployments, StatefulSets and DaemonSets of a namespace that you can work on, with " +
			"desired and ready replicas.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names, List: true,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace}, Args: obj,
				RunList: func(ctx context.Context, profile string) ([]engine.Item, error) {
					w, err := ws.Workloads(v.Environment, profile)
					if err != nil {
						return nil, err
					}
					list, err := w.ListWorkloads(ctx, v.Namespace)
					if err != nil {
						return nil, err
					}
					items := make([]engine.Item, 0, len(list))
					for _, s := range list {
						items = append(items, engine.Item{
							Scope: core.Scope{
								Environment: v.Environment, Namespace: v.Namespace, Kind: s.Kind, Name: s.Name,
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
