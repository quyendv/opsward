// Package capability defines what tools need from backends, independent of any one
// backend. Providers implement these interfaces.
package capability

import (
	"context"
	"errors"
	"time"

	"github.com/quyendv/opsward/internal/core"
)

// ErrNotFound is wrapped by providers when the requested object does not exist.
var ErrNotFound = errors.New("not found")

// ErrForbidden is wrapped by providers when the backend refuses the credential, as it
// does for a caller a profile acts as (docs/design/0014).
var ErrForbidden = core.ErrForbidden

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
	// Waiting is the reason a container is waiting, such as CrashLoopBackOff; for an init
	// container it is prefixed "Init:", as kubectl shows it.
	Waiting string `json:"waiting,omitempty"`
	// LastTerminated is the most recent termination of a container that has restarted:
	// how it ended (OOMKilled, Error) and with which exit code. OOM kills are reported
	// only here, never as events.
	LastTerminated *Termination `json:"lastTerminated,omitempty"`
}

// Termination is how a container instance ended.
type Termination struct {
	Container  string    `json:"container"`
	Reason     string    `json:"reason,omitempty"`
	ExitCode   int32     `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Event is a Kubernetes event about a workload or one of its children.
type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Object  string    `json:"object"` // Kind/name
	Message string    `json:"message"`
	Count   int32     `json:"count,omitempty"`
}

// LogOptions selects which logs WorkloadLogs reads.
type LogOptions struct {
	Pod        string // empty: every pod of the workload, up to MaxPods
	Container  string // empty: every init and regular container
	TailLines  int64
	Previous   bool // the container instance before the last restart
	MaxPods    int  // pods are read in name order; the rest are counted, not read
	MaxBytes   int  // per container; the newest output is kept, cut at a line boundary
	TotalBytes int  // across containers; containers past it are counted, not read
}

// ContainerLog is one container's recent output, or why it could not be read.
type ContainerLog struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Previous marks the output of the instance before the last restart, which can be
	// days old.
	Previous  bool     `json:"previous,omitempty"`
	Lines     []string `json:"lines,omitempty"`
	Truncated bool     `json:"truncated,omitempty"` // older lines were dropped to fit MaxBytes
	Error     string   `json:"error,omitempty"`
}

// WorkloadLogs is the recent output of a workload's containers.
type WorkloadLogs struct {
	Logs              []ContainerLog `json:"logs"`
	PodsOmitted       int            `json:"podsOmitted,omitempty"`
	ContainersOmitted int            `json:"containersOmitted,omitempty"` // not read: TotalBytes reached
}

// Workloads reads workloads in one cluster with one profile's credentials.
type Workloads interface {
	GetWorkload(ctx context.Context, ref WorkloadRef) (*Workload, error)
	// WorkloadLabels reads only the workload's labels, in one request. It runs before
	// authorization, so it must not cost more than that.
	WorkloadLabels(ctx context.Context, ref WorkloadRef) (map[string]string, error)
	// WorkloadMetadata reads the workload's labels and annotations, in one request.
	WorkloadMetadata(ctx context.Context, ref WorkloadRef) (*Metadata, error)
	// WorkloadEvents returns events of the workload, of ReplicaSets it controls and of
	// its pods, newest first, at most limit.
	WorkloadEvents(ctx context.Context, ref WorkloadRef, limit int) ([]Event, error)
	// WorkloadLogs returns recent output of the workload's pods' containers.
	WorkloadLogs(ctx context.Context, ref WorkloadRef, opt LogOptions) (*WorkloadLogs, error)
	// PodNamePattern returns an RE2 pattern (unanchored; metrics backends anchor it) that
	// matches the names of this workload's pods, current and past revisions, and no
	// other workload's.
	PodNamePattern(ctx context.Context, ref WorkloadRef) (string, error)
	// ListNamespaces returns the names of the cluster's namespaces, sorted.
	ListNamespaces(ctx context.Context) ([]string, error)
	// ListWorkloads returns a namespace's Deployments, StatefulSets and DaemonSets, or
	// every namespace's for "", sorted by namespace, kind, then name.
	ListWorkloads(ctx context.Context, namespace string) ([]WorkloadSummary, error)
	// WorkloadRestarts returns the workloads of a namespace, or of every namespace for "",
	// whose current pods' containers have restarted, sorted by namespace, kind, then name.
	WorkloadRestarts(ctx context.Context, namespace string) ([]WorkloadRestarts, error)
	// RestartWorkload restarts a workload's pods through its rollout, as kubectl rollout
	// restart does. With dryRun the change is previewed by the API server, not made.
	RestartWorkload(ctx context.Context, ref WorkloadRef, dryRun bool) (*WorkloadChange, error)
	// ScaleWorkload sets a Deployment's or StatefulSet's replicas. A workload that a
	// reconciler manages or an autoscaler targets is refused (docs/design/0007).
	ScaleWorkload(ctx context.Context, ref WorkloadRef, replicas int32, dryRun bool) (*WorkloadChange, error)
}

// Metadata is a workload's labels and annotations.
type Metadata struct {
	Labels      map[string]string
	Annotations map[string]string
}

// WorkloadChange is a narrow change to a workload, previewed or made.
type WorkloadChange struct {
	Diff string `json:"diff"`
	// Previous holds the prior values of the fields the change sets, to reverse it.
	Previous map[string]any `json:"previous"`
	// Reconciler names what manages the workload (Argo CD, Flux, Helm), if anything.
	Reconciler string `json:"reconciler,omitempty"`
}

// WorkloadSummary is one workload in a listing.
type WorkloadSummary struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	// Restarts and LastRestart are set when the listing asks for restarts.
	Restarts    int32      `json:"restarts,omitempty"`
	LastRestart *time.Time `json:"lastRestart,omitempty"`
	// Labels are for policy (apps scope) and are not returned.
	Labels map[string]string `json:"-"`
}

// WorkloadRestarts counts the container restarts of a workload's current pods, init
// containers included, and says when the last restarted container ended, if known.
type WorkloadRestarts struct {
	Namespace   string
	Kind        string
	Name        string
	Restarts    int32
	LastRestart *time.Time
}

// WorkloadSource hands out Workloads for an environment and profile.
type WorkloadSource interface {
	Workloads(environment, profile string) (Workloads, error)
}
