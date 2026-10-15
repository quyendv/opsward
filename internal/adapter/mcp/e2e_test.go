package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	mcpadapter "github.com/quyendv/opsward/internal/adapter/mcp"
	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

const e2eConfig = `
identity:
  principal: { subject: alice@example.com, groups: [team-a] }
storage:
  path: %DB%
audit:
  writer: test
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /unused }
environments:
  - name: staging
    cluster: cluster-a
    namespaces: ["team-a-*"]
policy:
  roles:
    viewer: { risk: [read] }
  bindings:
    - name: team-a-api-only
      subjects: { groups: [team-a] }
      role: viewer
      scope: { workloads: [api] }
`

// TestEndToEnd drives the real configuration, policy, engine, audit store, tool and
// Kubernetes provider through an MCP client, with only the cluster faked.
func TestEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "opsward.db")
	cfg, err := config.Parse([]byte(strings.Replace(e2eConfig, "%DB%", db, 1)))
	if err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	cluster := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team-a-shop"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team-a-shop"}},
	)
	var stream bytes.Buffer
	a, err := app.Open(ctx, cfg, app.Options{
		Stderr:        &stream,
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	st, ct := mcp.NewInMemoryTransports()
	p := a.Principal
	p.Adapter = mcpadapter.AdapterStdio
	if _, err := mcpadapter.NewServer(a.Engine, p).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	call := func(name, namespace string) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_workload_status", Arguments: map[string]any{
			"environment": "staging", "namespace": namespace, "kind": "Deployment", "name": name,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := call("api", "team-a-shop"); res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"ready": 1`) {
		t.Fatalf("allowed call: %+v", res.Content[0])
	}
	if res := call("worker", "team-a-shop"); !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "denied by policy") {
		t.Fatalf("workload outside the binding: %+v", res.Content[0])
	}
	if res := call("api", "kube-system"); !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not part of environment") {
		t.Fatalf("namespace outside the environment: %+v", res.Content[0])
	}

	records, err := audit.Load(ctx, a.Store, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("audited %d calls, want 3", len(records))
	}
	if err := audit.Verify(records); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	statuses := []string{records[0].Outcome.Status, records[1].Outcome.Status, records[2].Outcome.Status}
	if strings.Join(statuses, ",") != "ok,denied,invalid" {
		t.Fatalf("statuses = %v", statuses)
	}
	if lines := strings.Count(stream.String(), "\n"); lines != 3 {
		t.Fatalf("stream has %d records, want 3", lines)
	}
	var first audit.Record
	if err := json.Unmarshal([]byte(strings.SplitN(stream.String(), "\n", 2)[0]), &first); err != nil || first.Principal.Adapter != mcpadapter.AdapterStdio {
		t.Fatalf("first streamed record = %+v, %v", first, err)
	}
}

// TestEndToEndKnowledge: a crash-looping pod gets the builtin note attached through the
// real pipeline, and knowledge search works under a binding without scope restrictions.
func TestEndToEndKnowledge(t *testing.T) {
	ctx := context.Background()
	doc := strings.Replace(e2eConfig, "%DB%", filepath.Join(t.TempDir(), "opsward.db"), 1)
	doc = strings.Replace(doc, "    viewer: { risk: [read] }\n", "    viewer: { risk: [read] }\n    kb: { tools: [search_knowledge, get_knowledge] }\n", 1)
	doc += "    - name: knowledge\n      subjects: { groups: [team-a] }\n      role: kb\n"
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-a", Namespace: "team-a-shop", Labels: map[string]string{"app": "api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 9, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	cluster := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team-a-shop"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	}, crashing)
	a, err := app.Open(ctx, cfg, app.Options{
		Stderr:        &bytes.Buffer{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := mcpadapter.NewServer(a.Engine, a.Principal).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_workload_status", Arguments: map[string]any{
		"environment": "staging", "namespace": "team-a-shop", "kind": "Deployment", "name": "api",
	}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("status = %+v, %v", res, err)
	}
	if notes := res.Content[1].(*mcp.TextContent).Text; !strings.Contains(notes, "crashloop-backoff") {
		t.Fatalf("notes = %q", notes)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_knowledge", Arguments: map[string]any{"query": "image pull"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "image-pull-backoff") {
		t.Fatalf("search = %+v, %v", res, err)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_workload_logs", Arguments: map[string]any{
		"environment": "staging", "namespace": "team-a-shop", "kind": "Deployment", "name": "api", "previous": true,
	}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "fake logs") {
		t.Fatalf("logs = %+v, %v", res, err)
	}
}

// TestEndToEndMetricsAndLogs: backend tools through the real pipeline, with queries
// confined before they reach the backend and out-of-scope queries refused.
func TestEndToEndMetricsAndLogs(t *testing.T) {
	ctx := context.Background()
	var received []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.URL.Query().Get("query"))
		if strings.HasPrefix(r.URL.Path, "/loki/") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"api-a"},"values":[["1790000000000000000","boom"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":"api-a"},"value":[1790000000,"0.5"]}]}}`))
	}))
	defer backend.Close()
	doc := strings.Replace(e2eConfig, "%DB%", filepath.Join(t.TempDir(), "opsward.db"), 1)
	doc = strings.Replace(doc, "providers:\n", "providers:\n  metrics: { type: prometheus, url: "+backend.URL+" }\n  logs: { type: loki, url: "+backend.URL+" }\n", 1)
	doc = strings.Replace(doc, "      scope: { workloads: [api] }\n", "", 1) // namespace-wide calls need an unscoped binding
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(ctx, cfg, app.Options{
		Stderr:        &bytes.Buffer{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return fake.NewClientset(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := mcpadapter.NewServer(a.Engine, a.Principal).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	call := func(name string, args map[string]any) (bool, string) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, res.Content[0].(*mcp.TextContent).Text
	}

	isErr, text := call("query_metrics", map[string]any{"environment": "staging", "namespace": "team-a-shop", "query": "up"})
	if isErr || !strings.Contains(text, `"last": 0.5`) || !strings.Contains(received[0], `namespace="team-a-shop"`) {
		t.Fatalf("query_metrics: %v %s; backend got %q", isErr, text, received)
	}
	before := len(received)
	isErr, text = call("query_metrics", map[string]any{"environment": "staging", "namespace": "team-a-shop", "query": `up{namespace="kube-system"}`})
	if !isErr || !strings.Contains(text, "outside the call's scope") || len(received) != before {
		t.Fatalf("out-of-scope query: %v %s (backend requests %d → %d)", isErr, text, before, len(received))
	}
	isErr, text = call("query_logs", map[string]any{"environment": "staging", "namespace": "team-a-shop", "query": `{app="api"}`})
	if isErr || !strings.Contains(text, "boom") {
		t.Fatalf("query_logs: %v %s", isErr, text)
	}
	isErr, text = call("query_logs", map[string]any{"environment": "staging", "namespace": "team-a-shop", "query": `rate({app="api"}[5m])`})
	if !isErr || !strings.Contains(text, "only log queries") {
		t.Fatalf("metric LogQL: %v %s", isErr, text)
	}
}
