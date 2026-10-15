package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

// Limits from docs/design/0004.
const (
	defaultWindow   = time.Hour
	maxWindow       = 24 * time.Hour
	pointsPerSeries = 60
	maxQueryLength  = 4096
	defaultLogLines = 100
	maxLogLines     = 1000
)

// window parses a range such as "30m" or "6h"; empty means the default.
func window(s string) (time.Duration, error) {
	if s == "" {
		return defaultWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	if d <= 0 || d > maxWindow {
		return 0, fmt.Errorf("duration %s must be between 0 and %s", d, maxWindow)
	}
	return d, nil
}

// rangeQuery covers the last d with at most pointsPerSeries points.
func rangeQuery(query string, d time.Duration) capability.MetricsQuery {
	end := time.Now().UTC()
	step := (d + pointsPerSeries - 1) / pointsPerSeries
	step = step.Round(time.Second)
	if step < time.Second {
		step = time.Second
	}
	return capability.MetricsQuery{Query: query, Range: true, Start: end.Add(-d), End: end, Step: step}
}

type namespaceInput struct {
	Environment string `json:"environment" jsonschema:"name of a configured environment"`
	Namespace   string `json:"namespace" jsonschema:"namespace the query is confined to"`
}

func refineNamespaceInput(s *jsonschema.Schema) {
	s.Properties["environment"].MinLength = jsonschema.Ptr(1)
	s.Properties["namespace"].MinLength = jsonschema.Ptr(1)
}

type queryMetricsInput struct {
	namespaceInput
	Query string `json:"query" jsonschema:"PromQL expression; every selector is limited to the namespace (and cluster) automatically"`
	Range string `json:"range,omitempty" jsonschema:"for a range query, how far back from now, such as 1h; at most 24h. Omit for an instant query"`
}

// QueryMetrics is query_metrics.
func QueryMetrics(src capability.MetricsSource) (engine.Tool, error) {
	in, err := newInput[queryMetricsInput](func(s *jsonschema.Schema) {
		refineNamespaceInput(s)
		s.Properties["query"].MinLength = jsonschema.Ptr(1)
		s.Properties["query"].MaxLength = jsonschema.Ptr(maxQueryLength)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "query_metrics",
		Description: "Run a PromQL query, instant or over a range, limited to one namespace. Each series comes with " +
			"min, max and last. Prefer get_workload_metrics for a workload's CPU, memory or restarts.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			q := capability.MetricsQuery{Query: v.Query, Time: time.Now().UTC()}
			if v.Range != "" {
				d, err := window(v.Range)
				if err != nil {
					return nil, err
				}
				q = rangeQuery(v.Query, d)
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace},
				Args:  obj,
				Run: func(ctx context.Context, _ string) (any, error) {
					m, err := src.Metrics(v.Environment)
					if err != nil {
						return nil, err
					}
					return m.Query(ctx, v.Namespace, q)
				},
			}, nil
		},
	}, nil
}

type queryLogsInput struct {
	namespaceInput
	Query string `json:"query" jsonschema:"LogQL log query: one {stream selector} and an optional pipeline, such as {app=\"api\"} |= \"error\"; limited to the namespace automatically"`
	Since string `json:"since,omitempty" jsonschema:"how far back, such as 30m; default 1h, at most 24h"`
	Limit int    `json:"limit,omitempty" jsonschema:"newest lines to return; default 100"`
}

// QueryLogs is query_logs.
func QueryLogs(src capability.LogsSource) (engine.Tool, error) {
	in, err := newInput[queryLogsInput](func(s *jsonschema.Schema) {
		refineNamespaceInput(s)
		s.Properties["query"].MinLength = jsonschema.Ptr(1)
		s.Properties["query"].MaxLength = jsonschema.Ptr(maxQueryLength)
		s.Properties["limit"].Minimum = jsonschema.Ptr(1.0)
		s.Properties["limit"].Maximum = jsonschema.Ptr(float64(maxLogLines))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "query_logs",
		Description: "Search a namespace's logs with a LogQL log query, newest lines first, grouped by stream. " +
			"Metric queries over logs (rate, count_over_time) are not supported.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			since, err := window(v.Since)
			if err != nil {
				return nil, err
			}
			limit := v.Limit
			if limit == 0 {
				limit = defaultLogLines
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: v.Environment, Namespace: v.Namespace},
				Args:  obj,
				Run: func(ctx context.Context, _ string) (any, error) {
					l, err := src.Logs(v.Environment)
					if err != nil {
						return nil, err
					}
					return l.Query(ctx, v.Namespace, capability.LogsQuery{Query: v.Query, Since: since, Limit: limit})
				},
			}, nil
		},
	}, nil
}

type workloadMetricsInput struct {
	workloadInput
	Metric string `json:"metric" jsonschema:"cpu (cores), memory (working-set bytes) or restarts"`
	Range  string `json:"range,omitempty" jsonschema:"how far back from now; default 1h, at most 24h"`
}

type metricTemplate struct {
	unit, usage, resource string // resource: for requests and limits; empty when they do not apply
}

var metricTemplates = map[string]metricTemplate{
	"cpu":      {"cores", `sum by (pod) (rate(container_cpu_usage_seconds_total{pod=~%s, container!="", container!="POD"}[5m]))`, "cpu"},
	"memory":   {"bytes", `sum by (pod) (container_memory_working_set_bytes{pod=~%s, container!="", container!="POD"})`, "memory"},
	"restarts": {"count", `sum by (pod) (kube_pod_container_status_restarts_total{pod=~%s})`, ""},
}

// WorkloadMetricsResult is get_workload_metrics' result.
type WorkloadMetricsResult struct {
	Metric   string                    `json:"metric"`
	Unit     string                    `json:"unit"`
	Usage    *capability.MetricsResult `json:"usage"`
	Requests *capability.MetricsResult `json:"requests,omitempty"`
	Limits   *capability.MetricsResult `json:"limits,omitempty"`
}

// podPattern matches the names a workload's controller gives its pods.
func podPattern(kind, name string) string {
	n := regexp.QuoteMeta(name)
	switch kind {
	case "Deployment":
		return n + "-[a-z0-9]{5,10}-[a-z0-9]{5}"
	case "StatefulSet":
		return n + "-[0-9]+"
	default:
		return n + "-[a-z0-9]{5}"
	}
}

// GetWorkloadMetrics is get_workload_metrics.
func GetWorkloadMetrics(ws capability.WorkloadSource, ms capability.MetricsSource) (engine.Tool, error) {
	in, err := newInput[workloadMetricsInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["metric"].Enum = []any{"cpu", "memory", "restarts"}
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "get_workload_metrics",
		Description: "Show a workload's CPU, memory or restarts per pod over a time range, with min, max and last, and " +
			"for CPU and memory the pods' requests and limits.",
		Risk:        core.RiskRead,
		InputSchema: in.schema,
		Args:        in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			d, err := window(v.Range)
			if err != nil {
				return nil, err
			}
			labels := workloadPrepared(ws, v.workloadInput, obj, nil).Resolve
			return &engine.Prepared{
				Scope:   core.Scope{Environment: v.Environment, Namespace: v.Namespace, Kind: v.Kind, Name: v.Name},
				Args:    obj,
				Resolve: labels,
				Run: func(ctx context.Context, _ string) (any, error) {
					m, err := ms.Metrics(v.Environment)
					if err != nil {
						return nil, err
					}
					return workloadMetrics(ctx, m, v.Namespace, v.Kind, v.Name, v.Metric, d)
				},
			}, nil
		},
	}, nil
}

// workloadMetrics runs one metric's templates for a workload's pods.
func workloadMetrics(ctx context.Context, m capability.Metrics, namespace, kind, name, metric string, d time.Duration) (*WorkloadMetricsResult, error) {
	tpl := metricTemplates[metric]
	pods := strconv.Quote(podPattern(kind, name))
	res := &WorkloadMetricsResult{Metric: metric, Unit: tpl.unit}
	var err error
	if res.Usage, err = m.Query(ctx, namespace, rangeQuery(fmt.Sprintf(tpl.usage, pods), d)); err != nil {
		return nil, err
	}
	if tpl.resource != "" {
		// kube-state-metrics may be absent; requests and limits are then left out.
		res.Requests = optional(ctx, m, namespace, fmt.Sprintf(`sum by (pod) (kube_pod_container_resource_requests{pod=~%s, resource=%q})`, pods, tpl.resource))
		res.Limits = optional(ctx, m, namespace, fmt.Sprintf(`sum by (pod) (kube_pod_container_resource_limits{pod=~%s, resource=%q})`, pods, tpl.resource))
	}
	return res, nil
}

func optional(ctx context.Context, m capability.Metrics, namespace, query string) *capability.MetricsResult {
	r, err := m.Query(ctx, namespace, capability.MetricsQuery{Query: query, Time: time.Now().UTC()})
	if err != nil || len(r.Series) == 0 {
		return nil
	}
	return r
}
