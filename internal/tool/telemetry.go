package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

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
	// LogQL goes over GET: 2048 characters stay under common 8 KB URL limits once encoded.
	maxLogQueryLength = 2048
	defaultLogLines   = 100
	maxLogLines       = 1000
	// maxLogLineChars cuts single lines, such as structured JSON, that would crowd out
	// the rest; maxLogResultBytes bounds a log result, as JSON-encoded, to what an agent
	// can read at once: clients cap a tool result at some 25,000 tokens, and escaped JSON
	// is dense in tokens.
	maxLogLineChars   = 2000
	maxLogResultBytes = 32 << 10
)

// cutLine shortens a line beyond maxLogLineChars bytes, on a character boundary, and
// says by how many bytes.
func cutLine(s string) string {
	if len(s) <= maxLogLineChars {
		return s
	}
	end := maxLogLineChars
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return fmt.Sprintf("%s … [%d more bytes]", s[:end], len(s)-end)
}

// encodedLen is the length of s as a JSON string, the form the agent receives.
func encodedLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		return len(s)
	}
	return len(b)
}

// cutContainerLines cuts long lines of container logs, which are already bounded per
// container in bytes.
func cutContainerLines(logs []capability.ContainerLog) {
	for i := range logs {
		for j, l := range logs[i].Lines {
			logs[i].Lines[j] = cutLine(l)
		}
	}
}

// boundLogs cuts long lines and, past maxLogResultBytes, leaves out lines oldest first:
// streams come most recently active first, so their newest lines are kept first. A
// stream's labels count against the budget, and a stream left without lines is dropped.
func boundLogs(r *capability.LogsResult) {
	if r == nil {
		return
	}
	const perLine = 4 // the separators the encoding adds around a line
	budget := maxLogResultBytes
	kept := r.Streams[:0]
	for _, st := range r.Streams {
		lines := st.Lines
		labels, _ := json.Marshal(st.Labels)
		cost := len(labels) + 32 // labels plus the stream's own structure
		if budget < cost {
			r.LinesOmitted += len(lines)
			continue
		}
		budget -= cost
		keep := 0
		for j := len(lines) - 1; j >= 0; j-- {
			lines[j] = cutLine(lines[j])
			c := encodedLen(lines[j]) + perLine
			if budget < c {
				break
			}
			budget -= c
			keep++
		}
		r.LinesOmitted += len(lines) - keep
		if keep == 0 {
			budget += cost // the stream is not returned
			continue
		}
		st.Lines = lines[len(lines)-keep:]
		kept = append(kept, st)
	}
	r.Streams = kept
}

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

// rangeQuery covers the last d with at most pointsPerSeries points: a range [start, end]
// at step s has d/s+1 points, so the step is d/(points-1), rounded up to a whole second.
func rangeQuery(query string, d time.Duration) capability.MetricsQuery {
	end := time.Now().UTC()
	step := (d + pointsPerSeries - 2) / (pointsPerSeries - 1)
	if r := step % time.Second; r != 0 {
		step += time.Second - r
	}
	if step < time.Second {
		step = time.Second
	}
	return capability.MetricsQuery{Query: query, Range: true, Start: end.Add(-d), End: end, Step: step}
}

// queryScope is the scope of a query tool: a namespace, or the whole cluster where the
// environment and policy allow it (docs/design/0006).
type queryScope struct {
	Environment string `json:"environment" jsonschema:"name of a configured environment"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"namespace the query is confined to; omit for the whole cluster, where your permissions allow"`
}

func refineQueryScope(s *jsonschema.Schema) {
	s.Properties["environment"].MinLength = jsonschema.Ptr(1)
	s.Properties["namespace"].MinLength = jsonschema.Ptr(1)
}

type queryMetricsInput struct {
	queryScope
	Query  string `json:"query" jsonschema:"PromQL expression; every selector is limited to the namespace (if given) and the cluster automatically"`
	Range  string `json:"range,omitempty" jsonschema:"for a range query, how far back from now, such as 1h; at most 24h. Omit for an instant query"`
	Points bool   `json:"points,omitempty" jsonschema:"also return the points of each series (at most 60), not only min, max, avg and last"`
}

// summarize leaves only each series' summary: an overview needs no points.
func summarize(r *capability.MetricsResult) {
	if r == nil {
		return
	}
	for i := range r.Series {
		r.Series[i].Points, r.Series[i].PointsOmitted = nil, 0
	}
}

// QueryMetrics is query_metrics.
func QueryMetrics(src capability.MetricsSource) (engine.Tool, error) {
	in, err := newInput[queryMetricsInput](func(s *jsonschema.Schema) {
		refineQueryScope(s)
		s.Properties["query"].MinLength = jsonschema.Ptr(1)
		s.Properties["query"].MaxLength = jsonschema.Ptr(maxQueryLength)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "query_metrics",
		Description: "Run a PromQL query, instant or over a range, limited to one namespace, or to the whole cluster " +
			"without one. Each series comes with min, max, avg and last; points only on request. Prefer " +
			"get_workload_metrics for a workload's CPU, memory or restarts.",
		Risk:        core.RiskRead,
		ClusterWide: true,
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
					q.AllNamespaces = v.Namespace == ""
					r, err := m.Query(ctx, v.Namespace, q)
					if err == nil && !v.Points {
						summarize(r)
					}
					return r, err
				},
			}, nil
		},
	}, nil
}

type queryLogsInput struct {
	queryScope
	Query string `json:"query" jsonschema:"LogQL log query: one {stream selector} and an optional pipeline, such as {app=\"api\"} |= \"error\"; limited to the namespace automatically"`
	Since string `json:"since,omitempty" jsonschema:"how far back, such as 30m; default 1h, at most 24h"`
	Limit int    `json:"limit,omitempty" jsonschema:"newest lines to return; default 100"`
}

// QueryLogs is query_logs.
func QueryLogs(src capability.LogsSource) (engine.Tool, error) {
	in, err := newInput[queryLogsInput](func(s *jsonschema.Schema) {
		refineQueryScope(s)
		s.Properties["query"].MinLength = jsonschema.Ptr(1)
		s.Properties["query"].MaxLength = jsonschema.Ptr(maxLogQueryLength)
		s.Properties["limit"].Minimum = jsonschema.Ptr(1.0)
		s.Properties["limit"].Maximum = jsonschema.Ptr(float64(maxLogLines))
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "query_logs",
		Description: "Search a namespace's logs with a LogQL log query. Returns the newest matching lines, grouped by " +
			"stream (pod, container): the most recently active stream first, lines oldest first within a stream. " +
			"Metric queries over logs (rate, count_over_time) are not supported. Omit namespace to search the whole " +
			"cluster, where your permissions allow.",
		Risk:        core.RiskRead,
		ClusterWide: true,
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
					r, err := l.Query(ctx, v.Namespace, capability.LogsQuery{Query: v.Query, Since: since, Limit: limit, AllNamespaces: v.Namespace == ""})
					boundLogs(r)
					return r, err
				},
			}, nil
		},
	}, nil
}

type workloadMetricsInput struct {
	workloadInput
	Metric string `json:"metric" jsonschema:"cpu (cores), memory (working-set bytes) or restarts"`
	Range  string `json:"range,omitempty" jsonschema:"how far back from now; default 1h, at most 24h"`
	Points bool   `json:"points,omitempty" jsonschema:"also return the points of each series (at most 60), not only min, max, avg and last"`
}

type metricTemplate struct {
	unit, usage, resource string // resource: for requests and limits; empty when they do not apply
}

// Templates take the pod pattern as %[1]s and the rate window as %[2]s.
var metricTemplates = map[string]metricTemplate{
	"cpu":      {"cores", `sum by (pod) (rate(container_cpu_usage_seconds_total{pod=~%[1]s, container!="", container!="POD"}[%[2]s]))`, "cpu"},
	"memory":   {"bytes", `sum by (pod) (container_memory_working_set_bytes{pod=~%[1]s, container!="", container!="POD"})`, "memory"},
	"restarts": {"count", `sum by (pod) (kube_pod_container_status_restarts_total{pod=~%[1]s})`, ""},
}

// minRateWindow is the shortest rate window; longer steps widen it so that every interval
// between two points is covered.
const minRateWindow = 5 * time.Minute

// WorkloadMetricsResult is get_workload_metrics' result.
type WorkloadMetricsResult struct {
	Metric   string                    `json:"metric"`
	Unit     string                    `json:"unit"`
	Usage    *capability.MetricsResult `json:"usage"`
	Requests *capability.MetricsResult `json:"requests,omitempty"`
	Limits   *capability.MetricsResult `json:"limits,omitempty"`
	// Unavailable says why requests or limits failed; they are simply absent when
	// kube-state-metrics provides none.
	Unavailable map[string]string `json:"unavailable,omitempty"`
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
		Needs:       []string{capability.OpWorkloadsGet, capability.OpWorkloadsPods},
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
				Run: func(ctx context.Context, profile string) (any, error) {
					w, err := ws.Workloads(v.Environment, profile)
					if err != nil {
						return nil, err
					}
					pods, err := w.PodNamePattern(ctx, capability.WorkloadRef{Namespace: v.Namespace, Kind: v.Kind, Name: v.Name})
					if err != nil {
						return nil, err
					}
					m, err := ms.Metrics(v.Environment)
					if err != nil {
						return nil, err
					}
					r, err := workloadMetrics(ctx, m, v.Namespace, pods, v.Metric, d)
					if err == nil && !v.Points {
						summarize(r.Usage)
						summarize(r.Requests)
						summarize(r.Limits)
					}
					return r, err
				},
			}, nil
		},
	}, nil
}

// workloadMetrics runs one metric's templates for the pods podPattern matches.
func workloadMetrics(ctx context.Context, m capability.Metrics, namespace, podPattern, metric string, d time.Duration) (*WorkloadMetricsResult, error) {
	tpl := metricTemplates[metric]
	pods := strconv.Quote(podPattern)
	res := &WorkloadMetricsResult{Metric: metric, Unit: tpl.unit}
	q := rangeQuery("", d)
	window := max(q.Step, minRateWindow)
	q.Query = fmt.Sprintf(tpl.usage, pods, fmt.Sprintf("%ds", int(window.Seconds())))
	var err error
	if res.Usage, err = m.Query(ctx, namespace, q); err != nil {
		return nil, err
	}
	if tpl.resource != "" {
		for name, dst := range map[string]**capability.MetricsResult{"requests": &res.Requests, "limits": &res.Limits} {
			r, err := m.Query(ctx, namespace, capability.MetricsQuery{
				Query: fmt.Sprintf(`sum by (pod) (kube_pod_container_resource_%s{pod=~%s, resource=%q})`, name, pods, tpl.resource),
				Time:  time.Now().UTC(),
			})
			switch {
			case err != nil:
				if res.Unavailable == nil {
					res.Unavailable = map[string]string{}
				}
				res.Unavailable[name] = err.Error()
			case len(r.Series) > 0:
				*dst = r
			}
		}
	}
	return res, nil
}
