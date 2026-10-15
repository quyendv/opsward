package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	t    *testing.T
	path string
	db   string
	app  *App
}

func (f *fixture) write(group string, extra string) {
	f.t.Helper()
	doc := strings.NewReplacer("%DB%", f.db, "%GROUP%", group).Replace(base) + extra
	if err := os.WriteFile(f.path, []byte(doc), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeReplacing(group, old, replacement string) {
	f.t.Helper()
	doc := strings.NewReplacer("%DB%", f.db, "%GROUP%", group).Replace(base)
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
	f.app = a
	return f
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
	if got := strings.Join(names, ","); got != "get_knowledge,get_workload_status,list_workload_events,list_workload_logs,search_knowledge" {
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
