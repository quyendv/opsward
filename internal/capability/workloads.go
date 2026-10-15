// Package capability defines what tools need from backends, independent of any one
// backend. Providers implement these interfaces.
package capability

import (
	"context"
	"errors"
)

// ErrNotFound is wrapped by providers when the requested object does not exist.
var ErrNotFound = errors.New("not found")

// WorkloadKinds are the kinds the Workloads capability understands.
var WorkloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

// WorkloadRef names one workload.
type WorkloadRef struct {
	Namespace string
	Kind      string
	Name      string
}

// Workload is a workload's status and the pods it selects.
type Workload struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Labels are read for policy and never returned to the caller.
	Labels     map[string]string `json:"-"`
	Replicas   Replicas          `json:"replicas"`
	Conditions []Condition       `json:"conditions,omitempty"`
	Pods       []Pod             `json:"pods"`
}

// Replicas summarizes rollout progress.
type Replicas struct {
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Updated   int32 `json:"updated"`
	Available int32 `json:"available"`
}

// Condition is a workload status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Pod is one pod's state, reduced to what diagnosis needs.
type Pod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Node     string `json:"node,omitempty"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	// Waiting is the reason a container is waiting, such as CrashLoopBackOff.
	Waiting string `json:"waiting,omitempty"`
}

// Workloads reads workloads in one cluster with one profile's credentials.
type Workloads interface {
	GetWorkload(ctx context.Context, ref WorkloadRef) (*Workload, error)
	// WorkloadLabels reads only the workload's labels, in one request. It runs before
	// authorization, so it must not cost more than that.
	WorkloadLabels(ctx context.Context, ref WorkloadRef) (map[string]string, error)
}

// WorkloadSource hands out Workloads for an environment and profile.
type WorkloadSource interface {
	Workloads(environment, profile string) (Workloads, error)
}
