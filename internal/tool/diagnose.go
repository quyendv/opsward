package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

// Limits of diagnose_workload, kept small: it is the first look, not the full read.
const (
	diagnoseEvents   = 20
	diagnosePods     = 3
	diagnoseTail     = 50
	diagnoseLogBytes = 8 << 10
	diagnoseLogTotal = 24 << 10
	// diagnoseRecent is how long ago a container may have crashed for a pod that is
	// healthy now to count as failing: older crash logs would read as current.
	diagnoseRecent = time.Hour
	// diagnoseMetricsBudget bounds the whole metrics part, whatever each request's timeout.
	diagnoseMetricsBudget = 20 * time.Second
)

// Diagnosis is diagnose_workload's result. Unavailable names each part that could not be
// produced and why; a missing part does not fail the call.
type Diagnosis struct {
	Status      *capability.Workload              `json:"status"`
	Events      []capability.Event                `json:"events,omitempty"`
	Logs        []capability.ContainerLog         `json:"logs,omitempty"`
	Metrics     map[string]*WorkloadMetricsResult `json:"metrics,omitempty"`
	Unavailable map[string]string                 `json:"unavailable,omitempty"`
}

// DiagnoseWorkload is diagnose_workload. ms may be nil when no metrics provider is configured.
func DiagnoseWorkload(ws capability.WorkloadSource, ms capability.MetricsSource) (engine.Tool, error) {
	in, err := newInput[workloadInput](refineWorkloadInput)
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "diagnose_workload",
		Description: "First look at an unhealthy workload in one call: status, recent events, the last lines of the " +
			"failing containers (the crashed instance, marked previous, when a container restarted within the hour) and, when metrics are available, " +
			"CPU and memory over the last hour. Parts that cannot be read are listed under unavailable.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return workloadPrepared(ws, v, obj, func(ctx context.Context, w capability.Workloads, ref capability.WorkloadRef) (any, error) {
				return diagnose(ctx, w, ms, v.Environment, ref)
			}), nil
		},
	}, nil
}

// isFailing reports whether a pod is failing now or had a container crash recently. A
// termination without a time counts as recent.
func isFailing(p capability.Pod, now time.Time) bool {
	if !p.Ready || p.Waiting != "" {
		return true
	}
	t := p.LastTerminated
	return t != nil && (t.FinishedAt.IsZero() || now.Sub(t.FinishedAt) < diagnoseRecent)
}

func diagnose(ctx context.Context, w capability.Workloads, ms capability.MetricsSource, env string, ref capability.WorkloadRef) (*Diagnosis, error) {
	status, err := w.GetWorkload(ctx, ref)
	if err != nil {
		return nil, err
	}
	d := &Diagnosis{Status: status, Unavailable: map[string]string{}}

	if d.Events, err = w.WorkloadEvents(ctx, ref, diagnoseEvents); err != nil {
		d.Unavailable["events"] = err.Error()
	}

	failing := 0
	for _, p := range status.Pods {
		if !isFailing(p, time.Now()) {
			continue
		}
		if failing++; failing > diagnosePods {
			break
		}
		opt := capability.LogOptions{Pod: p.Name, TailLines: diagnoseTail, MaxPods: 1, MaxBytes: diagnoseLogBytes, TotalBytes: diagnoseLogTotal}
		if p.LastTerminated != nil {
			// The crashed instance holds the error; the current one may not have failed yet.
			opt.Container, opt.Previous = p.LastTerminated.Container, true
		}
		logs, err := w.WorkloadLogs(ctx, ref, opt)
		if err != nil {
			d.Unavailable["logs."+p.Name] = err.Error()
			continue
		}
		cutContainerLines(logs.Logs)
		d.Logs = append(d.Logs, logs.Logs...)
	}

	if ms == nil {
		d.Unavailable["metrics"] = "no metrics provider configured"
	} else if m, err := ms.Metrics(env); err != nil {
		d.Unavailable["metrics"] = err.Error()
	} else if pods, err := w.PodNamePattern(ctx, ref); err != nil {
		d.Unavailable["metrics"] = err.Error()
	} else {
		mctx, cancel := context.WithTimeout(ctx, diagnoseMetricsBudget)
		defer cancel()
		d.Metrics = map[string]*WorkloadMetricsResult{}
		for _, metric := range []string{"cpu", "memory"} {
			r, err := workloadMetrics(mctx, m, ref.Namespace, pods, metric, time.Hour)
			if err != nil {
				// A backend that failed once (timeout, outage) is not asked again.
				d.Unavailable["metrics"] = err.Error()
				break
			}
			summarize(r.Usage)
			summarize(r.Requests)
			summarize(r.Limits)
			d.Metrics[metric] = r
		}
		if len(d.Metrics) == 0 {
			d.Metrics = nil
		}
	}
	if len(d.Unavailable) == 0 {
		d.Unavailable = nil
	}
	return d, nil
}
