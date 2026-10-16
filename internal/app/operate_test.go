package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

const operateDoc = `
identity:
  mode: remote
  groups:
    platform: [lead@example.com]
    team-a: [alice@example.com]
server:
  http: { address: 127.0.0.1:0 }
approvals: { ttl: 30m }
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
          operator: { kubeconfig: /unused }
environments:
  - name: staging
    cluster: cluster-a
policy:
  roles:
    scaler: { tools: [scale_workload] }
    approver: { tools: [list_approvals, decide_approval] }
  bindings:
    - name: team-a-scale-with-approval
      subjects: { groups: [team-a] }
      role: scaler
      effect: approve
      approvers: { groups: [platform] }
    - name: platform-approves
      subjects: { groups: [platform] }
      role: approver
`

// Scaling through an approval, end to end on the real pipeline: request, decide, retry.
func TestScaleThroughApproval(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(strings.Replace(operateDoc, "%DB%", filepath.Join(dir, "o.db"), 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approvals.TTL.Std() != 30*time.Minute {
		t.Fatalf("approvals.ttl = %v", cfg.Approvals.TTL)
	}
	two := int32(2)
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two}})
	// The fake applies dry runs; an API server does not.
	cluster.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return len(a.(k8stesting.PatchActionImpl).PatchOptions.DryRun) > 0, nil, nil
	})
	a, err := Open(context.Background(), cfg, Options{Stderr: &strings.Builder{}, ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	ctx := context.Background()
	alice := core.Principal{Subject: "alice@example.com", Groups: []string{"team-a"}, Adapter: "test"}
	lead := core.Principal{Subject: "lead@example.com", Groups: []string{"platform"}, Adapter: "test"}
	args := `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","replicas":4}`

	_, err = a.Engine.Invoke(ctx, alice, "scale_workload", json.RawMessage(args))
	var req *engine.ApprovalRequiredError
	if !errors.As(err, &req) || req.Diff != "spec.replicas: 2 -> 4" || time.Until(req.ExpiresAt) > 31*time.Minute {
		t.Fatalf("err = %v", err)
	}
	res, err := a.Engine.Invoke(ctx, lead, "list_approvals", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(mustJSONString(t, res.Data), `"canDecide":true`) {
		t.Fatalf("list_approvals = %+v, %v", res, err)
	}
	if _, err := a.Engine.Invoke(ctx, lead, "decide_approval", json.RawMessage(`{"id":"`+req.ID+`","decision":"approve","reason":"load test"}`)); err != nil {
		t.Fatal(err)
	}
	retry := strings.Replace(args, "}", `,"approval":"`+req.ID+`"}`, 1)
	res, err = a.Engine.Invoke(ctx, alice, "scale_workload", json.RawMessage(retry))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	d, _ := cluster.AppsV1().Deployments("shop").Get(ctx, "api", metav1.GetOptions{})
	if *d.Spec.Replicas != 4 {
		t.Fatalf("replicas = %d", *d.Spec.Replicas)
	}
	if !strings.Contains(mustJSONString(t, res.Data), `"previous":{"spec.replicas":2}`) {
		t.Fatalf("result = %s", mustJSONString(t, res.Data))
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// In local mode nobody can approve, so the approval tools are not offered.
func TestLocalModeHasNoApprovalTools(t *testing.T) {
	f := newFixture(t, "team-a")
	for _, tl := range f.app.Engine.Tools() {
		if strings.Contains(tl.Name, "approval") {
			t.Fatalf("local mode registers %s", tl.Name)
		}
	}
}
