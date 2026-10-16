package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/quyendv/opsward/internal/config"
)

type promStub struct {
	mu      sync.Mutex
	queries []string
}

func (p *promStub) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.queries = append(p.queries, r.FormValue("query"))
		p.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Each environment's queries go to the backend its cluster uses: its own, unlabelled, or
// the shared one, with the cluster's label.
func TestQueriesGoToTheClustersBackend(t *testing.T) {
	own, shared := &promStub{}, &promStub{}
	ownSrv, sharedSrv := own.server(t), shared.server(t)
	doc := `
identity:
  principal: { subject: alice@example.com, groups: [team-a] }
storage:
  path: ` + filepath.Join(t.TempDir(), "o.db") + `
providers:
  metrics: { type: prometheus, url: ` + sharedSrv.URL + `, clusterLabel: cluster }
  kubernetes:
    clusters:
      cluster-a:
        profiles: { reader: { kubeconfig: /unused } }
        metrics: { type: prometheus, url: ` + ownSrv.URL + ` }
      cluster-b:
        profiles: { reader: { kubeconfig: /unused } }
      cluster-c:
        profiles: { reader: { kubeconfig: /unused } }
environments:
  - { name: staging, cluster: cluster-a }
  - { name: production, cluster: cluster-b }
  - { name: other, cluster: cluster-c }
policy:
  roles: { viewer: { risk: [read] } }
  bindings: [{ name: all, subjects: { groups: [team-a] }, role: viewer }]
`
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), cfg, Options{Stderr: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	for _, env := range []string{"staging", "production"} {
		if _, err := a.Engine.Invoke(context.Background(), a.Principal, "query_metrics", json.RawMessage(`{"environment":"`+env+`","namespace":"shop","query":"up"}`)); err != nil {
			t.Fatalf("%s: %v", env, err)
		}
	}
	if len(own.queries) != 1 || own.queries[0] != `up{namespace="shop"}` {
		t.Fatalf("own backend got %v", own.queries)
	}
	if len(shared.queries) != 1 || !strings.Contains(shared.queries[0], `cluster="cluster-b"`) {
		t.Fatalf("shared backend got %v", shared.queries)
	}
	res, err := a.Engine.Invoke(context.Background(), a.Principal, "list_environments", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Data)
	if !strings.Contains(string(b), `"cluster":"cluster-c","logs":false,"metrics":true,"name":"other"`) {
		t.Fatalf("environments = %s", b)
	}
}
