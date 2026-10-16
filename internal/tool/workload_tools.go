package tool

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

// Limits from docs/design/0003.
const (
	defaultEventLimit = 50
	maxEventLimit     = 200
	defaultTailLines  = 100
	maxTailLines      = 1000
	maxLogPods        = 10
	// Bounded so that a result fits what an agent reads at once (see maxLogResultBytes).
	maxLogBytes      = 16 << 10 // per container
	maxLogTotalBytes = 32 << 10 // per call
)

type eventsInput struct {
	workloadInput
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of events to return, newest first; default 50"`
}

// WorkloadEvents is list_workload_events.
func WorkloadEvents(src capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[eventsInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["limit"].Minimum = jsonschema.Ptr(1.0)
		s.Properties["limit"].Maximum = jsonschema.Ptr(float64(maxEventLimit))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "list_workload_events",
		Description: "List recent Kubernetes events of a workload, of the ReplicaSets a Deployment owns, and of its pods, " +
			"newest first. Scheduling failures, image pull errors, failed probes and failed pod creation appear here; " +
			"OOM kills do not (see lastTerminated in get_workload_status).",
		Risk:        core.RiskRead,
		Needs:       []string{capability.OpWorkloadsGet, capability.OpWorkloadsPods, capability.OpWorkloadsEvents},
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			limit := v.Limit
			if limit == 0 {
				limit = defaultEventLimit
			}
			return workloadPrepared(src, v.workloadInput, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
				return w.WorkloadEvents(ctx, ref, limit)
			}), nil
		},
	}, nil
}

type logsInput struct {
	workloadInput
	Pod       string `json:"pod,omitempty" jsonschema:"one pod of the workload; default the first 10 pods by name"`
	Container string `json:"container,omitempty" jsonschema:"one container, init containers included; default every container of each pod"`
	TailLines int64  `json:"tailLines,omitempty" jsonschema:"lines from the end of each container's log; default 100"`
	Previous  bool   `json:"previous,omitempty" jsonschema:"read the container instance before its last restart, which holds the crash"`
}

// WorkloadLogs is list_workload_logs.
func WorkloadLogs(src capability.WorkloadSource) (engine.Tool, error) {
	in, err := newInput[logsInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["pod"].MinLength = jsonschema.Ptr(1)
		s.Properties["container"].MinLength = jsonschema.Ptr(1)
		s.Properties["tailLines"].Minimum = jsonschema.Ptr(1.0)
		s.Properties["tailLines"].Maximum = jsonschema.Ptr(float64(maxTailLines))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "list_workload_logs",
		Description: "Read the last lines of each container (init containers included) of a workload's pods: at most 10 pods " +
			"in name order, or one pod with pod=. Use previous=true for a container in CrashLoopBackOff: the current " +
			"instance has usually not failed yet. Long output keeps the newest lines and is marked truncated.",
		Risk:        core.RiskRead,
		Needs:       []string{capability.OpWorkloadsGet, capability.OpWorkloadsPods, capability.OpWorkloadsLogs},
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			opt := capability.LogOptions{
				Pod: v.Pod, Container: v.Container, TailLines: v.TailLines, Previous: v.Previous,
				MaxPods: maxLogPods, MaxBytes: maxLogBytes, TotalBytes: maxLogTotalBytes,
			}
			if opt.TailLines == 0 {
				opt.TailLines = defaultTailLines
			}
			return workloadPrepared(src, v.workloadInput, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
				logs, err := w.WorkloadLogs(ctx, ref, opt)
				if err == nil {
					cutContainerLines(logs.Logs)
				}
				return logs, err
			}), nil
		},
	}, nil
}
