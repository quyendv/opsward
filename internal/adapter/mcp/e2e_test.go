package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
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
