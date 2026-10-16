package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/engine"
)

type fakeMetrics struct {
	env, namespace string
	queries        []capability.MetricsQuery
	deadlines      []bool
	fail           *struct {
		match string
		err   error
	}
}

func (f *fakeMetrics) Metrics(env string) (capability.Metrics, error) { f.env = env; return f, nil }

func (f *fakeMetrics) Query(ctx context.Context, ns string, q capability.MetricsQuery) (*capability.MetricsResult, error) {
	_, hasDeadline := ctx.Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)
	f.namespace = ns
	f.queries = append(f.queries, q)
	if f.fail != nil && strings.Contains(q.Query, f.fail.match) {
		return nil, f.fail.err
	}
	one := 1.0
	return &capability.MetricsResult{Query: q.Query, Series: []capability.Series{{
		Labels: map[string]string{"pod": "api-a"}, Last: &one, Samples: 1,
		Points: []capability.Point{{Time: q.End, Value: one}},
	}}}, nil
}

type fakeLogs struct {
	namespace string
	q         capability.LogsQuery
	result    *capability.LogsResult
}

func (f *fakeLogs) Logs(string) (capability.Logs, error) { return f, nil }

func (f *fakeLogs) Query(_ context.Context, ns string, q capability.LogsQuery) (*capability.LogsResult, error) {
	f.namespace, f.q = ns, q
	if f.result != nil {
		return f.result, nil
	}
	return &capability.LogsResult{Query: q.Query}, nil
}

func runTool(t *testing.T, tl engine.Tool, args string) (*engine.Prepared, any, error) {
	t.Helper()
	p, err := tl.Prepare(json.RawMessage(args))
	if err != nil {
		return nil, nil, err
	}
	out, err := p.Run(context.Background(), engine.ProfileReader)
	return p, out, err
}

func TestQueryMetrics(t *testing.T) {
	m := &fakeMetrics{}
	tl, err := QueryMetrics(m)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"up"}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Environment != "production" || p.Scope.Namespace != "shop" || p.Scope.Name != "" || p.Resolve != nil {
		t.Fatalf("scope = %+v", p.Scope)
	}
	if m.env != "production" || m.namespace != "shop" || m.queries[0].Range {
		t.Fatalf("instant query = %+v", m.queries[0])
	}
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"up","range":"1h"}`); err != nil {
		t.Fatal(err)
	}
	q := m.queries[1]
	if points := int(q.End.Sub(q.Start)/q.Step) + 1; !q.Range || q.End.Sub(q.Start) != time.Hour || points > 60 {
		t.Fatalf("range query = %+v", q)
	}
	for _, bad := range []string{
		`{"environment":"production","namespace":"shop","query":"up","range":"25h"}`,
		`{"environment":"production","namespace":"shop","query":"up","range":"soon"}`,
		`{"environment":"production","namespace":"shop","query":""}`,
		`{"environment":"production","namespace":"shop","query":"` + strings.Repeat("u", 4097) + `"}`,
	} {
		if _, _, err := runTool(t, tl, bad); err == nil {
			t.Errorf("accepted %.80s", bad)
		}
	}
}

func TestQueryLogs(t *testing.T) {
	l := &fakeLogs{}
	tl, err := QueryLogs(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"{app=\"api\"}"}`); err != nil {
		t.Fatal(err)
	}
	if l.namespace != "shop" || l.q.Since != time.Hour || l.q.Limit != 100 {
		t.Fatalf("defaults = %+v", l.q)
	}
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"{app=\"api\"}","since":"30m","limit":5}`); err != nil || l.q.Since != 30*time.Minute || l.q.Limit != 5 {
		t.Fatalf("explicit = %+v, %v", l.q, err)
	}
	for _, bad := range []string{
		`{"environment":"production","namespace":"shop","query":"{a=\"b\"}","limit":1001}`,
		`{"environment":"production","namespace":"shop","query":"{a=\"b\"}","since":"48h"}`,
	} {
		if _, _, err := runTool(t, tl, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestGetWorkloadMetrics(t *testing.T) {
	m := &fakeMetrics{}
	ws := &fakeSource{workload: &capability.Workload{Labels: map[string]string{"app.kubernetes.io/part-of": "shop"}}}
	tl, err := GetWorkloadMetrics(ws, m)
	if err != nil {
		t.Fatal(err)
	}
	p, out, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"memory"}`)
	if err != nil {
		t.Fatal(err)
	}
	if labels, err := p.Resolve(context.Background()); err != nil || labels["app.kubernetes.io/part-of"] != "shop" {
		t.Fatalf("Resolve = %v, %v", labels, err)
	}
	if len(m.queries) != 3 {
		t.Fatalf("memory runs usage, requests and limits; got %d queries", len(m.queries))
	}
	usage := m.queries[0]
	if !usage.Range || !strings.Contains(usage.Query, "container_memory_working_set_bytes") || !strings.Contains(usage.Query, `pod=~"api-7d9f8b6c5-[bcdfghjklmnpqrstvwxz2456789]{5}"`) {
		t.Fatalf("usage query = %+v", usage)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"unit":"bytes"`) || !strings.Contains(string(b), `"requests"`) {
		t.Fatalf("result = %s", b)
	}

	m.queries = nil
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"StatefulSet","name":"db.main","metric":"restarts"}`); err != nil {
		t.Fatal(err)
	}
	if len(m.queries) != 1 || !strings.Contains(m.queries[0].Query, "kube_pod_container_status_restarts_total") {
		t.Fatalf("restarts = %+v", m.queries)
	}
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"disk"}`); err == nil {
		t.Fatal("unknown metric accepted")
	}
}

// Issue #15: the CPU rate window covers the whole step, so spikes between points count.
func TestCPUWindowFollowsTheStep(t *testing.T) {
	m := &fakeMetrics{}
	tl, _ := GetWorkloadMetrics(&fakeSource{workload: &capability.Workload{}}, m)
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"cpu","range":"24h"}`); err != nil {
		t.Fatal(err)
	}
	q := m.queries[0]
	if want := fmt.Sprintf("[%ds]", int(q.Step.Seconds())); !strings.Contains(q.Query, want) {
		t.Fatalf("24h CPU query %q lacks window %s", q.Query, want)
	}
	m.queries = nil
	if _, _, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"cpu","range":"10m"}`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.queries[0].Query, "[300s]") {
		t.Fatalf("short-range CPU query %q should keep a 5m window", m.queries[0].Query)
	}
}

// Issue #18: a failed requests or limits query says why, unlike an absent one.
func TestRequestsAndLimitsReportErrors(t *testing.T) {
	m := &fakeMetrics{fail: &struct {
		match string
		err   error
	}{"kube_pod_container_resource_limits", errors.New("timeout")}}
	tl, _ := GetWorkloadMetrics(&fakeSource{workload: &capability.Workload{}}, m)
	_, out, err := runTool(t, tl, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"memory"}`)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*WorkloadMetricsResult)
	if r.Requests == nil || r.Limits != nil || !strings.Contains(r.Unavailable["limits"], "timeout") {
		t.Fatalf("result = %+v", r)
	}
}

// LogQL still goes over GET, so its query is kept under common 8 KB URL limits.
func TestQueryLogsLengthLimit(t *testing.T) {
	tl, _ := QueryLogs(&fakeLogs{})
	long, _ := json.Marshal(map[string]string{"environment": "production", "namespace": "shop", "query": "{a=\"b\"} |= \"" + strings.Repeat("x", 2048) + "\""})
	if _, err := tl.Prepare(long); err == nil {
		t.Fatal("a query over 2048 characters was accepted")
	}
}

// An overview needs min, max, avg and last; every point costs tokens.
func TestMetricsAreSummariesUnlessPointsAreAsked(t *testing.T) {
	m := &fakeMetrics{}
	qm, _ := QueryMetrics(m)
	_, out, err := runTool(t, qm, `{"environment":"production","namespace":"shop","query":"up","range":"1h"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s := out.(*capability.MetricsResult).Series[0]; len(s.Points) != 0 || s.Samples != 1 || s.Last == nil {
		t.Fatalf("default series = %+v", s)
	}
	_, out, _ = runTool(t, qm, `{"environment":"production","namespace":"shop","query":"up","range":"1h","points":true}`)
	if s := out.(*capability.MetricsResult).Series[0]; len(s.Points) != 1 {
		t.Fatalf("points: true = %+v", s)
	}
	wm, _ := GetWorkloadMetrics(&fakeSource{workload: &capability.Workload{}}, m)
	_, out, err = runTool(t, wm, `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api","metric":"cpu"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*WorkloadMetricsResult); len(r.Usage.Series[0].Points) != 0 || len(r.Requests.Series[0].Points) != 0 {
		t.Fatalf("workload metrics = %+v", r)
	}
}

// Without a namespace, a query is cluster-wide: the engine decides whether it may be.
func TestQueryToolsAcceptClusterWideCalls(t *testing.T) {
	m, l := &fakeMetrics{}, &fakeLogs{}
	qm, _ := QueryMetrics(m)
	ql, _ := QueryLogs(l)
	for _, tl := range []engine.Tool{qm, ql} {
		if !tl.ClusterWide {
			t.Fatalf("%s is not cluster-wide", tl.Name)
		}
	}
	p, _, err := runTool(t, qm, `{"environment":"production","query":"sum(node_load1)"}`)
	if err != nil || p.Scope.Namespace != "" || m.namespace != "" {
		t.Fatalf("query_metrics without namespace: %+v, %v", p, err)
	}
	if _, _, err := runTool(t, ql, `{"environment":"production","query":"{app=\"api\"}"}`); err != nil {
		t.Fatalf("query_logs without namespace: %v", err)
	}
}

// Long log lines (structured JSON) and many of them must fit what an agent can read: each
// line is cut, and the oldest lines go first when the whole result is too large.
func TestQueryLogsBoundsLineLengthAndResultSize(t *testing.T) {
	long := strings.Repeat("x", 5000)
	var lines []string
	for i := range 600 {
		lines = append(lines, fmt.Sprintf("line-%03d %s", i, strings.Repeat("y", 1500)))
	}
	lines = append(lines, long)
	l := &fakeLogs{result: &capability.LogsResult{Streams: []capability.LogStream{{Labels: map[string]string{"pod": "a"}, Lines: lines}}}}
	tl, _ := QueryLogs(l)
	_, out, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"{a=\"b\"}","limit":1000}`)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*capability.LogsResult)
	if b, _ := json.Marshal(r); len(b) > maxLogResultBytes+4096 {
		t.Fatalf("result is %d bytes encoded", len(b))
	}
	got := r.Streams[0].Lines
	last := got[len(got)-1]
	if len(last) > maxLogLineChars+64 || !strings.Contains(last, "more characters") {
		t.Fatalf("long line kept as %d chars: %.80s", len(last), last)
	}
	if r.LinesOmitted == 0 || strings.HasPrefix(got[0], "line-000") {
		t.Fatalf("omitted %d; first kept line %.20s: the oldest should go first", r.LinesOmitted, got[0])
	}
}

// The budget is what the agent receives: JSON-encoded, where every quote of a structured
// log line doubles.
func TestQueryLogsBudgetCountsEncodedSize(t *testing.T) {
	var lines []string
	for i := range 400 {
		lines = append(lines, fmt.Sprintf(`{"i":%d,"level":"ERROR","msg":"%s"}`, i, strings.Repeat(`\"q\" `, 60)))
	}
	l := &fakeLogs{result: &capability.LogsResult{Streams: []capability.LogStream{{Labels: map[string]string{"pod": "a"}, Lines: lines}}}}
	tl, _ := QueryLogs(l)
	_, out, err := runTool(t, tl, `{"environment":"production","namespace":"shop","query":"{a=\"b\"}","limit":1000}`)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); len(b) > maxLogResultBytes+4096 {
		t.Fatalf("encoded result is %d bytes", len(b))
	}
}
