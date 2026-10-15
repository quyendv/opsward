package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
	"github.com/quyendv/opsward/internal/store/storetest"
)

const replicaDoc = `
identity:
  mode: remote
server:
  http: { address: 127.0.0.1:0 }
storage:
  type: postgres
  dsn: { env: OPSWARD_APP_TEST_DSN }
audit:
  writer: shared
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /unused }
environments:
  - name: staging
    cluster: cluster-a
policy:
  roles:
    viewer: { risk: [read] }
  bindings:
    - name: everyone
      role: viewer
`

// Two replicas on one Postgres share tokens and one audit chain.
func TestReplicasShareTokensAndChain(t *testing.T) {
	t.Setenv("OPSWARD_APP_TEST_DSN", storetest.PostgresSchema(t))
	ctx := context.Background()

	cfg, err := config.Parse([]byte(replicaDoc))
	if err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}})
	replica := func() *App {
		a, err := Open(ctx, cfg, Options{Stderr: &strings.Builder{}, ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	r1, r2 := replica(), replica()
	tok, _, err := r1.Identity.CreateToken(ctx, identity.TokenRequest{Subject: "alice@example.com", TTL: time.Hour, MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := r2.Identity.Authenticate(ctx, tok)
	if err != nil {
		t.Fatalf("a token created on one replica does not work on the other: %v", err)
	}
	for i := range 10 {
		r := []*App{r1, r2}[i%2]
		if _, err := r.Engine.Invoke(ctx, p, "get_workload_status", json.RawMessage(statusArgs)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := audit.Load(ctx, r1.Store, "shared")
	if err != nil || len(records) != 10 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	if err := audit.Verify(records); err != nil {
		t.Fatalf("shared chain: %v", err)
	}
	if err := r2.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}
