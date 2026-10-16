package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/source"
)

// Bounds of get_delivery_status (docs/design/0010).
const (
	maxDeliveryResources  = 50
	maxDeliveryHistory    = 5
	maxDeliveryConditions = 10
	maxDeliveryMessage    = 1024
)

// DeliveryView is get_delivery_status's answer.
type DeliveryView struct {
	// Managed is false when no Argo CD Application manages the workload; Note says so.
	Managed           bool                `json:"managed"`
	Note              string              `json:"note,omitempty"`
	Application       string              `json:"application,omitempty"`
	Sync              *DeliverySync       `json:"sync,omitempty"`
	Health            *DeliveryHealth     `json:"health,omitempty"`
	Operation         *DeliveryOperation  `json:"operation,omitempty"`
	Conditions        []DeliveryCondition `json:"conditions,omitempty"`
	ConditionsOmitted int                 `json:"conditionsOmitted,omitempty"`
	// Resources are those in the workload's namespace that are out of sync or not
	// healthy; ResourcesElsewhere counts such resources in other namespaces or
	// cluster-wide, which the caller is not authorized to see by name.
	Resources          []DeliveryResource `json:"resources,omitempty"`
	ResourcesOmitted   int                `json:"resourcesOmitted,omitempty"`
	ResourcesElsewhere int                `json:"resourcesElsewhere,omitempty"`
	History            []DeliveryRevision `json:"history,omitempty"`
}

// DeliverySync compares the live state with Git.
// A multi-source Application has a revision per source, in Revisions and
// TargetRevisions.
type DeliverySync struct {
	Status          string   `json:"status"`
	Revision        string   `json:"revision,omitempty"`       // live
	Revisions       []string `json:"revisions,omitempty"`      // live, per source
	TargetRevision  string   `json:"targetRevision,omitempty"` // as the Application asks
	TargetRevisions []string `json:"targetRevisions,omitempty"`
}

// DeliveryHealth is the Application's health.
type DeliveryHealth struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// DeliveryOperation is the last sync operation.
type DeliveryOperation struct {
	Phase      string   `json:"phase"`
	Message    string   `json:"message,omitempty"`
	StartedAt  string   `json:"startedAt,omitempty"`
	FinishedAt string   `json:"finishedAt,omitempty"`
	Revision   string   `json:"revision,omitempty"`
	Revisions  []string `json:"revisions,omitempty"`
}

// DeliveryCondition is an Application condition, such as a comparison or sync error.
type DeliveryCondition struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// DeliveryResource is a managed resource that is out of sync or not healthy.
type DeliveryResource struct {
	Group         string `json:"group,omitempty"`
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name"`
	Sync          string `json:"sync,omitempty"`
	Health        string `json:"health,omitempty"`
	HealthMessage string `json:"healthMessage,omitempty"`
}

// DeliveryRevision is one deployment.
type DeliveryRevision struct {
	Revision   string   `json:"revision,omitempty"`
	Revisions  []string `json:"revisions,omitempty"` // multi-source
	DeployedAt string   `json:"deployedAt,omitempty"`
}

// DeliveryStatus is get_delivery_status.
func DeliveryStatus(src capability.WorkloadSource, cfg ChangeConfig) (engine.Tool, error) {
	in, err := newInput[workloadInput](refineWorkloadInput)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "get_delivery_status",
		Description: "Show how Argo CD sees a workload: whether it is in sync with Git, its health, the last sync " +
			"and its errors, the resources out of sync or unhealthy, and recent deployments. Use it after a change " +
			"is merged to see whether it has landed.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names,
		Needs: []string{capability.OpWorkloadsGet},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return workloadPrepared(src, v, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
				m, err := w.WorkloadMetadata(ctx, ref)
				if err != nil {
					return nil, err
				}
				managed, err := source.Managing(ctx, source.Workload{
					Environment: v.Environment, Namespace: v.Namespace, Kind: v.Kind, Name: v.Name,
					Labels: m.Labels, Annotations: m.Annotations,
				}, cfg.Changes().Resolve)
				if err != nil {
					return nil, err
				}
				if managed == nil {
					return &DeliveryView{Note: "no Argo CD Application manages this workload"}, nil
				}
				return deliveryView(managed, v.Namespace), nil
			}), nil
		},
	}, nil
}

func deliveryView(m *source.Managed, namespace string) *DeliveryView {
	st := m.App.Status
	v := &DeliveryView{
		Managed: true, Application: m.Namespace + "/" + m.Name,
		Sync:   &DeliverySync{Status: st.Sync.Status, Revision: st.Sync.Revision, Revisions: st.Sync.Revisions},
		Health: &DeliveryHealth{Status: st.Health.Status, Message: bound(st.Health.Message)},
	}
	if targets := m.App.TargetRevisions(); len(targets) == 1 {
		v.Sync.TargetRevision = targets[0]
	} else {
		v.Sync.TargetRevisions = targets
	}
	if op := st.OperationState; op != nil {
		v.Operation = &DeliveryOperation{Phase: op.Phase, Message: bound(op.Message), StartedAt: op.StartedAt, FinishedAt: op.FinishedAt}
		if op.SyncResult != nil {
			v.Operation.Revision, v.Operation.Revisions = op.SyncResult.Revision, op.SyncResult.Revisions
		}
	}
	for _, c := range st.Conditions {
		// A shared resource warning names other Applications and their namespaces.
		if c.Type == "SharedResourceWarning" {
			continue
		}
		if len(v.Conditions) == maxDeliveryConditions {
			v.ConditionsOmitted++
			continue
		}
		v.Conditions = append(v.Conditions, DeliveryCondition{Type: c.Type, Message: bound(c.Message)})
	}
	if st.ResourceHealthSource == "appTree" {
		v.Note = "Argo CD does not record the health of each resource in the Application; resources show their sync status only"
	}
	// Unhealthy resources come first: they are what a diagnosis needs, and what the
	// bound would otherwise cut.
	var unhealthy, outOfSync []DeliveryResource
	for _, r := range st.Resources {
		dr := DeliveryResource{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Sync: r.Status}
		if r.Health != nil {
			dr.Health, dr.HealthMessage = r.Health.Status, bound(r.Health.Message)
		}
		problem := (dr.Health != "" && dr.Health != "Healthy") || (dr.Sync != "" && dr.Sync != "Synced")
		switch {
		case !problem:
		case r.Namespace != namespace:
			v.ResourcesElsewhere++
		case dr.Health != "" && dr.Health != "Healthy":
			unhealthy = append(unhealthy, dr)
		default:
			outOfSync = append(outOfSync, dr)
		}
	}
	all := slices.Concat(unhealthy, outOfSync)
	if len(all) > maxDeliveryResources {
		v.ResourcesOmitted = len(all) - maxDeliveryResources
		all = all[:maxDeliveryResources]
	}
	v.Resources = all
	for _, h := range slices.Backward(st.History) {
		if len(v.History) == maxDeliveryHistory {
			break
		}
		v.History = append(v.History, DeliveryRevision{Revision: h.Revision, Revisions: h.Revisions, DeployedAt: h.DeployedAt})
	}
	return v
}

// bound cuts a message from Argo CD, which may hold a whole render error, to a size an
// agent can read.
func bound(msg string) string {
	if len(msg) <= maxDeliveryMessage {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxDeliveryMessage], "") + " … (truncated)"
}
