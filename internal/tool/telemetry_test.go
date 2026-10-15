package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/engine"
)

type fakeMetrics struct {
	env, namespace string
	queries        []capability.MetricsQuery
}

func (f *fakeMetrics) Metrics(env string) (capability.Metrics, error) { f.env = env; return f, nil }

func (f *fakeMetrics) Query(_ context.Context, ns string, q capability.MetricsQuery) (*capability.MetricsResult, error) {
	f.namespace = ns
	f.queries = append(f.queries, q)
	return &capability.MetricsResult{Query: q.Query, Series: []capability.Series{{Labels: map[string]string{"pod": "api-a"}, Last: 1}}}, nil
}

type fakeLogs struct {
	namespace string
	q         capability.LogsQuery
}

func (f *fakeLogs) Logs(string) (capability.Logs, error) { return f, nil }

func (f *fakeLogs) Query(_ context.Context, ns string, q capability.LogsQuery) (*capability.LogsResult, error) {
	f.namespace, f.q = ns, q
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
	if !q.Range || q.End.Sub(q.Start) != time.Hour || q.Step != time.Minute {
		t.Fatalf("range query = %+v", q)
	}
	for _, bad := range []string{
		`{"environment":"production","namespace":"shop","query":"up","range":"25h"}`,
		`{"environment":"production","namespace":"shop","query":"up","range":"soon"}`,
		`{"environment":"production","query":"up"}`,
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
