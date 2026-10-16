package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

// giteaRepo serves one repository, team-a/gitops, with one file, through the Gitea API.
func giteaRepo(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/team-a/gitops")
		switch p {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case "/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"id": "c0ffee"}})
		case "/git/trees/c0ffee":
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]any{{"path": "apps/api/values.yaml", "type": "blob"}}})
		case "/contents/apps/api/values.yaml":
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("replicaCount: 2\n"))})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func changeFixture(t *testing.T) *fixture {
	t.Helper()
	srv := giteaRepo(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("t0ken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "team-a")
	f.extra = "  forges:\n    - { name: git, type: gitea, url: " + srv.URL + ", hosts: [git.example.com], token: { file: " + token + " }, repositories: [team-a/gitops] }\n"
	f.reopen()
	return f
}

const static = "sourceResolution:\n  static:\n    - { environment: staging, namespace: shop, name: api, source: \"git:team-a/gitops//apps/api\" }\n"

const proposeArgs = `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","title":"Scale api",` +
	`"edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 3"}]}`

func TestChangeToolsNeedAForge(t *testing.T) {
	f := newFixture(t, "team-a")
	for _, tl := range f.app.Engine.Tools() {
		if tl.Name == "propose_change" || tl.Name == "resolve_source" {
			t.Fatalf("%s registered without a forge", tl.Name)
		}
	}
}

// A role limited to read may suggest, and may not open pull requests: the strategy sets
// the call's risk before authorization.
func TestProposeChangeRiskFollowsTheStrategy(t *testing.T) {
	f := changeFixture(t)
	res, err := f.app.Engine.Invoke(t.Context(), f.app.Principal, "propose_change", json.RawMessage(proposeArgs))
	if b, _ := json.Marshal(res); err != nil || !strings.Contains(string(b), "source not resolved") {
		t.Fatalf("without a source: %s, %v", b, err)
	}
	f.write("team-a", static)
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatal(err)
	}
	res, err = f.app.Engine.Invoke(t.Context(), f.app.Principal, "propose_change", json.RawMessage(proposeArgs))
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if b, _ := json.Marshal(res.Data); !strings.Contains(string(b), "+replicaCount: 3") {
		t.Fatalf("suggest result = %s", b)
	}
	if err := f.call("resolve_source", `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}`); err != nil {
		t.Fatalf("resolve_source: %v", err)
	}

	f.envExtra = "    change: { strategy: pull-request }\n"
	f.write("team-a", static)
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.call("propose_change", proposeArgs); !denied(err) {
		t.Fatalf("pull-request with a read-only role: err = %v, want denied", err)
	}
}

// get_delivery_status needs providers.delivery and no forge; the change tools need a
// forge.
func TestDeliveryToolNeedsOnlyDelivery(t *testing.T) {
	f := newFixture(t, "team-a")
	f.extra = "  delivery: { type: argocd, cluster: cluster-a }\n"
	f.reopen()
	var names []string
	for _, tl := range f.app.Engine.Tools() {
		names = append(names, tl.Name)
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "get_delivery_status") || strings.Contains(got, "propose_change") {
		t.Fatalf("tools = %s", got)
	}
}

// Environments on the delivery cluster accept Applications deploying in-cluster; others
// only the destinations configured for their cluster.
func TestDeliveryDestinationsPerEnvironment(t *testing.T) {
	doc := strings.Replace(base, "%DB%", filepath.Join(t.TempDir(), "o.db"), 1)
	doc = strings.Replace(doc, "%GROUP%", "team-a", 1)
	doc = strings.Replace(doc, "          reader: { kubeconfig: /unused }\n",
		"          reader: { kubeconfig: /unused }\n      cluster-b:\n        profiles:\n          reader: { kubeconfig: /unused }\n      cluster-c:\n        profiles:\n          reader: { kubeconfig: /unused }\n"+
			"  delivery: { type: argocd, cluster: cluster-a, destinations: { cluster-b: [https://b.example.com] } }\n", 1)
	doc = strings.Replace(doc, "environments:\n", "environments:\n  - { name: production, cluster: cluster-b }\n  - { name: lab, cluster: cluster-c }\n", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	c, err := openChanges(cfg, kubernetes.NewSource(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Changes().Resolve.Destinations
	if !slices.Contains(d["staging"], "in-cluster") || !slices.Equal(d["production"], []string{"https://b.example.com"}) || len(d["lab"]) != 0 {
		t.Fatalf("destinations = %v", d)
	}
}
