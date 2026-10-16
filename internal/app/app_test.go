package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

const base = `
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
policy:
  roles:
    viewer: { risk: [read] }
  bindings:
    - name: grant
      subjects: { groups: [%GROUP%] }
      role: viewer
`

type fixture struct {
	t        *testing.T
	path     string
	db       string
	app      *App
	extra    string // provider lines inserted under providers:
	envExtra string // lines appended to the staging environment
}

func (f *fixture) doc(group string) string {
	doc := strings.NewReplacer("%DB%", f.db, "%GROUP%", group).Replace(base)
	doc = strings.Replace(doc, "providers:\n", "providers:\n"+f.extra, 1)
	return strings.Replace(doc, "    cluster: cluster-a\n", "    cluster: cluster-a\n"+f.envExtra, 1)
}

func (f *fixture) write(group string, extra string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(f.doc(group)+extra), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeAll() { f.write("team-a", "") }

// reopen writes the current fixture configuration and opens a new App on it.
func (f *fixture) reopen() {
	f.t.Helper()
	_ = f.app.Close()
	f.writeAll()
	cfg, err := config.Load(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.app = open(f.t, f, cfg)
}

func (f *fixture) writeReplacing(group, old, replacement string) {
	f.t.Helper()
	doc := f.doc(group)
	if !strings.Contains(doc, old) {
		f.t.Fatalf("fixture has no %q", old)
	}
	if err := os.WriteFile(f.path, []byte(strings.Replace(doc, old, replacement, 1)), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func newFixture(t *testing.T, group string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, path: filepath.Join(dir, "opsward.yaml"), db: filepath.Join(dir, "o.db")}
	f.write(group, "")
	cfg, err := config.Load(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.app = open(t, f, cfg)
	return f
}

func open(t *testing.T, f *fixture, cfg *config.Config) *App {
	t.Helper()
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}})
	a, err := Open(context.Background(), cfg, Options{
		Stderr:        &strings.Builder{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
		ConfigPath:    f.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func (f *fixture) call(tool, args string) error {
	_, err := f.app.Engine.Invoke(context.Background(), f.app.Principal, tool, json.RawMessage(args))
	return err
}

const statusArgs = `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}`

func denied(err error) bool {
	var d *engine.DeniedError
	return errors.As(err, &d)
}

func TestOpenRegistersM2Tools(t *testing.T) {
	f := newFixture(t, "team-a")
	var names []string
	for _, tl := range f.app.Engine.Tools() {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "diagnose_workload,get_knowledge,get_workload_status,list_environments,list_namespaces,list_workload_events,list_workload_logs,list_workloads,search_knowledge" {
		t.Fatalf("tools = %s", got)
	}
	if err := f.call("search_knowledge", `{"query":"crash"}`); err != nil {
		t.Fatalf("search_knowledge: %v", err)
	}
}

func TestReloadAppliesPolicy(t *testing.T) {
	f := newFixture(t, "nobody")
	if err := f.call("get_workload_status", statusArgs); !denied(err) {
		t.Fatalf("before reload: err = %v, want denied", err)
	}
	f.write("team-a", "")
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatalf("ReloadFile: %v", err)
	}
	if err := f.call("get_workload_status", statusArgs); err != nil {
		t.Fatalf("after reload: %v", err)
	}
}

func TestReloadKeepsOldStateOnRejection(t *testing.T) {
	f := newFixture(t, "team-a")
	for name, tt := range map[string]struct {
		mutate func()
		want   string
	}{
		"restart-only change": {func() { f.writeReplacing("team-a", "  writer: test\n", "  writer: test\n  timeout: 9s\n") }, "restart"},
		"invalid policy":      {func() { f.write("team-a", "  extra: [") }, "config"},
		"unknown role":        {func() { f.write("team-a", "    - {name: b2, role: missing}\n") }, `unknown role "missing"`},
		"bad knowledge path":  {func() { f.write("team-a", "knowledge:\n  paths: [/does/not/exist]\n") }, "does/not/exist"},
	} {
		t.Run(name, func(t *testing.T) {
			tt.mutate()
			if err := f.app.ReloadFile(f.path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReloadFile err = %v, want %q", err, tt.want)
			}
			if err := f.call("get_workload_status", statusArgs); err != nil {
				t.Fatalf("previous configuration not in force: %v", err)
			}
		})
	}
}

func TestReloadLoadsKnowledge(t *testing.T) {
	f := newFixture(t, "team-a")
	kb := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(kb, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := "---\nid: local-runbook\ntitle: Local runbook\n---\nRestart the thing.\n"
	if err := os.WriteFile(filepath.Join(kb, "r.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.call("get_knowledge", `{"id":"local-runbook"}`); err == nil {
		t.Fatal("document found before it was configured")
	}
	f.write("team-a", "knowledge:\n  paths: ["+kb+"]\n")
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.call("get_knowledge", `{"id":"local-runbook"}`); err != nil {
		t.Fatalf("after reload: %v", err)
	}
}

func watch(t *testing.T, f *fixture, interval time.Duration) (chan os.Signal, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { f.app.Watch(ctx, interval, hup); close(done) }()
	return hup, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Watch did not stop when its context ended")
		}
	}
}

func TestWatchReloadsWhenTheFileChanges(t *testing.T) {
	f := newFixture(t, "nobody")
	_, stop := watch(t, f, 20*time.Millisecond)
	defer stop()
	f.write("team-a", "")
	eventually(t, func() bool { return f.call("get_workload_status", statusArgs) == nil })
}

func TestWatchReloadsOnSignal(t *testing.T) {
	f := newFixture(t, "nobody")
	hup, stop := watch(t, f, time.Hour) // the poller never fires in this test
	defer stop()
	f.write("team-a", "")
	time.Sleep(50 * time.Millisecond)
	if err := f.call("get_workload_status", statusArgs); !denied(err) {
		t.Fatalf("reloaded without a signal: %v", err)
	}
	hup <- syscall.SIGHUP
	eventually(t, func() bool { return f.call("get_workload_status", statusArgs) == nil })
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Issue #3: a change written between parsing the configuration and Open is applied, because
// Open records the hash of what was parsed, not of the file as it is when Open runs.
func TestWatchAppliesAChangeMadeDuringStartUp(t *testing.T) {
	dir := t.TempDir()
	f := &fixture{t: t, path: filepath.Join(dir, "opsward.yaml"), db: filepath.Join(dir, "o.db")}
	f.write("nobody", "")
	parsed, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(parsed)
	if err != nil {
		t.Fatal(err)
	}
	f.write("team-a", "") // the change lands before Open
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}})
	a, err := Open(context.Background(), cfg, Options{
		Stderr:        &strings.Builder{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
		ConfigPath:    f.path,
		ConfigHash:    ContentHash(parsed),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	f.app = a
	_, stop := watch(t, f, 20*time.Millisecond)
	defer stop()
	eventually(t, func() bool { return f.call("get_workload_status", statusArgs) == nil })
}

// fakeBackend answers Prometheus and Loki queries and records the queries it received.
type fakeBackend struct {
	mu      sync.Mutex
	queries []string
}

func (b *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.queries = append(b.queries, r.FormValue("query"))
	b.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/loki/") {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
}

func (b *fakeBackend) last() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queries[len(b.queries)-1]
}

func backendsFixture(t *testing.T, clusterValue string) (*fixture, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	f := newFixture(t, "team-a")
	f.extra = "  metrics: { type: prometheus, url: " + srv.URL + ", clusterLabel: cluster }\n  logs: { type: loki, url: " + srv.URL + " }\n"
	f.envExtra = clusterValue
	f.reopen()
	return f, b
}

func TestBackendToolsAreRegisteredOnlyWhenConfigured(t *testing.T) {
	f, _ := backendsFixture(t, "")
	var names []string
	for _, tl := range f.app.Engine.Tools() {
		names = append(names, tl.Name)
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"get_workload_metrics", "query_metrics", "query_logs", "diagnose_workload"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools %s missing %s", got, want)
		}
	}
}

func TestQueriesUseTheEnvironmentsClusterValueAfterReload(t *testing.T) {
	f, b := backendsFixture(t, "")
	args := `{"environment":"staging","namespace":"shop","query":"up"}`
	if err := f.call("query_metrics", args); err != nil {
		t.Fatal(err)
	}
	if q := b.last(); !strings.Contains(q, `cluster="cluster-a"`) || !strings.Contains(q, `namespace="shop"`) {
		t.Fatalf("query = %s", q)
	}
	f.envExtra = "    clusterLabelValue: prod-a\n"
	f.writeAll()
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.call("query_metrics", args); err != nil {
		t.Fatal(err)
	}
	if q := b.last(); !strings.Contains(q, `cluster="prod-a"`) {
		t.Fatalf("after reload: %s", q)
	}
	if err := f.call("diagnose_workload", statusArgs); err != nil {
		t.Fatalf("diagnose with backends: %v", err)
	}
}

func TestCheckDoesNotNeedBackendFiles(t *testing.T) {
	doc := strings.NewReplacer("%DB%", "/tmp/x.db", "%GROUP%", "team-a").Replace(base)
	doc = strings.Replace(doc, "providers:\n", "providers:\n  metrics: { type: prometheus, url: https://m.example.com, caFile: /missing/ca.pem }\n", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(cfg); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// list_environments follows the configuration, and policy decides each environment.
func TestListEnvironmentsFollowsReload(t *testing.T) {
	f := newFixture(t, "team-a")
	names := func() string {
		t.Helper()
		res, err := f.app.Engine.Invoke(context.Background(), f.app.Principal, "list_environments", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range res.Data.([]any) {
			out = append(out, e.(map[string]any)["name"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "staging" {
		t.Fatalf("environments = %s", got)
	}
	f.writeReplacing("team-a", "policy:\n", "  - name: production\n    cluster: cluster-a\npolicy:\n")
	if err := f.app.ReloadFile(f.path); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "staging,production" {
		t.Fatalf("after reload, environments = %s", got)
	}
}
