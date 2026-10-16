package config

import (
	"strings"
	"testing"
)

const ownBackends = `
identity:
  principal: { subject: me }
storage:
  path: /var/lib/opsward/opsward.db
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles: { reader: { kubeconfig: /etc/opsward/a } }
        metrics: { type: prometheus, url: https://prometheus.cluster-a.example.com }
        logs: { type: loki, url: https://loki.cluster-a.example.com }
      cluster-b:
        profiles: { reader: { kubeconfig: /etc/opsward/b } }
environments:
  - name: staging
    cluster: cluster-a
  - name: production
    cluster: cluster-b
`

// Each cluster with its own backend needs no cluster label: the backend holds one cluster.
func TestClusterOwnBackends(t *testing.T) {
	c, err := Parse([]byte(ownBackends))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Providers.Kubernetes.Clusters["cluster-a"]
	if a.Metrics == nil || a.Metrics.NamespaceLabel != "namespace" || a.Logs == nil {
		t.Fatalf("cluster-a backends = %+v %+v", a.Metrics, a.Logs)
	}
	if b, ok := c.MetricsBackend("production"); ok || b != nil {
		t.Fatalf("production has no metrics backend, got %+v", b)
	}
	if b, ok := c.MetricsBackend("staging"); !ok || b.URL != "https://prometheus.cluster-a.example.com" {
		t.Fatalf("staging metrics backend = %+v", b)
	}
}

// A shared backend that serves several clusters must tell them apart; one that serves a
// single cluster, because the others have their own, need not.
func TestSharedBackendClusterLabel(t *testing.T) {
	shared := strings.Replace(ownBackends, "providers:\n", "providers:\n  metrics: { type: prometheus, url: https://metrics.example.com }\n", 1)
	c, err := Parse([]byte(shared))
	if err != nil {
		t.Fatalf("shared backend used by one cluster: %v", err)
	}
	if b, _ := c.MetricsBackend("production"); b == nil || b.URL != "https://metrics.example.com" {
		t.Fatalf("production falls back to the shared backend: %+v", b)
	}
	if b, _ := c.MetricsBackend("staging"); b.URL != "https://prometheus.cluster-a.example.com" {
		t.Fatalf("staging keeps its own backend: %+v", b)
	}
	both := strings.Replace(shared, "        metrics: { type: prometheus, url: https://prometheus.cluster-a.example.com }\n", "", 1)
	if _, err := Parse([]byte(both)); err == nil || !strings.Contains(err.Error(), "providers.metrics.clusterLabel is required") {
		t.Fatalf("shared by two clusters without a label: %v", err)
	}
}

func TestClusterBackendValidation(t *testing.T) {
	bad := strings.Replace(ownBackends, "https://loki.cluster-a.example.com", "loki.cluster-a", 1)
	if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "providers.kubernetes.clusters.cluster-a.logs.url") {
		t.Fatalf("err = %v", err)
	}
}

func TestClusterBackendsAreRestartOnly(t *testing.T) {
	base, _ := Parse([]byte(ownBackends))
	next, err := Parse([]byte(strings.Replace(ownBackends, "https://prometheus.cluster-a.example.com", "https://prometheus2.cluster-a.example.com", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(base.RestartRequired(next), ","); got != "providers.kubernetes.clusters.cluster-a.metrics" {
		t.Fatalf("RestartRequired = %q", got)
	}
}
