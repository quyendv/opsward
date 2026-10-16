package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/notify"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

// An alert from Alertmanager is investigated through the engine as the alerts principal,
// under policy, and the diagnosis reaches the webhook.
func TestAlertIsInvestigatedAndNotified(t *testing.T) {
	got := make(chan notify.Notification, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n notify.Notification
		_ = json.NewDecoder(r.Body).Decode(&n)
		got <- n
	}))
	defer hook.Close()
	dir := t.TempDir()
	doc := strings.Replace(remoteDoc, "%DB%", filepath.Join(dir, "o.db"), 1) + `
policy:
  roles: { diagnose: { tools: [diagnose_workload] } }
  bindings: [{ name: alerts, subjects: { users: [alertmanager] }, role: diagnose }]
alerts:
  principal: alertmanager
  notify: [{ type: webhook, url: ` + hook.URL + ` }]
`
	path := filepath.Join(dir, "opsward.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	})
	stream := &lockedBuilder{}
	a, err := Open(context.Background(), cfg, Options{
		Stderr: stream, ConfigPath: path,
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if a.Alerts == nil {
		t.Fatal("no alert receiver")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Alerts.Run(ctx)

	body := `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"KubePodCrashLooping","namespace":"shop","deployment":"api"}}]}`
	w := httptest.NewRecorder()
	a.Alerts.Handle(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alerts/alertmanager", strings.NewReader(body)), core.Principal{Subject: "alertmanager"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("response %d %s", w.Code, w.Body)
	}
	select {
	case n := <-got:
		if n.Error != "" || n.Workload == nil || n.Workload.Environment != "staging" || n.Diagnosis == nil {
			t.Fatalf("notification = %+v", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	// Another principal's delivery is refused, and audited as a denial.
	w = httptest.NewRecorder()
	a.Alerts.Handle(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alerts/alertmanager", strings.NewReader(body)), core.Principal{Subject: "mallory@example.com"})
	if w.Code != http.StatusForbidden || !strings.Contains(stream.String(), `"rule":"alerts.principal"`) {
		t.Fatalf("refusal: %d, audit:\n%s", w.Code, stream.String())
	}
	// The investigation is audited as the alerts principal, through the alertmanager adapter.
	if out := stream.String(); !strings.Contains(out, `"subject":"alertmanager"`) || !strings.Contains(out, `"adapter":"alertmanager"`) ||
		!strings.Contains(out, `"tool":"diagnose_workload"`) {
		t.Fatalf("audit stream:\n%s", out)
	}
}

// lockedBuilder is an audit stream written from the receiver's goroutines.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Alerts whose principal no binding lets diagnose would all be denied: validate says so.
func TestCheckWarnsWhenAlertsCannotDiagnose(t *testing.T) {
	doc := strings.Replace(remoteDoc, "%DB%", "/var/lib/opsward/o.db", 1) + `
alerts:
  principal: alertmanager
  notify: [{ type: webhook, url: https://hooks.example.com/x }]
`
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := Check(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(warnings, "\n"); !strings.Contains(w, "no binding lets alertmanager run diagnose_workload") {
		t.Fatalf("warnings:\n%s", w)
	}
	cfg, err = config.Parse([]byte(doc + "policy:\n  roles: { d: { tools: [diagnose_workload] } }\n  bindings: [{ name: a, subjects: { users: [alertmanager] }, role: d }]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if warnings, _ := Check(cfg); strings.Contains(strings.Join(warnings, "\n"), "no binding lets") {
		t.Fatalf("warned although a binding allows it: %v", warnings)
	}
}

// fakeAnthropic answers the Messages API: first a tool call, then an answer.
func fakeAnthropic(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		n := len(seen)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"t1","name":"get_workload_status","input":{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"No pods are running: the Deployment has no replicas ready."}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":9}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestAlertIsAnalyzedByTheAgent(t *testing.T) {
	model, seen := fakeAnthropic(t)
	got := make(chan notify.Notification, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n notify.Notification
		_ = json.NewDecoder(r.Body).Decode(&n)
		got <- n
	}))
	defer hook.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("k3y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(remoteDoc, "%DB%", filepath.Join(dir, "o.db"), 1) + `
policy:
  roles: { investigate: { tools: [diagnose_workload, get_workload_status] } }
  bindings: [{ name: alerts, subjects: { users: [alertmanager] }, role: investigate }]
agent:
  provider: anthropic
  model: claude-sonnet-5-5
  url: ` + model.URL + `
  apiKey: { file: ` + key + ` }
alerts:
  principal: alertmanager
  agent: true
  notify: [{ type: webhook, url: ` + hook.URL + ` }]
`
	path := filepath.Join(dir, "opsward.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	})
	stream := &lockedBuilder{}
	a, err := Open(context.Background(), cfg, Options{
		Stderr: stream, ConfigPath: path,
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Alerts.Run(ctx)
	body := `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"KubeDeploymentReplicasMismatch","namespace":"shop","deployment":"api"}}]}`
	a.Alerts.Handle(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alerts/alertmanager", strings.NewReader(body)), core.Principal{Subject: "alertmanager"})
	select {
	case n := <-got:
		if n.Analysis != "No pods are running: the Deployment has no replicas ready." {
			t.Fatalf("notification = %+v", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	// The model was told the alert and the diagnosis; only read tools were offered, and
	// its call went through the engine as the alerts principal, adapter agent.
	first, _ := json.Marshal((*seen)[0])
	if !strings.Contains(string(first), "KubeDeploymentReplicasMismatch") || strings.Contains(string(first), `"name":"restart_workload"`) {
		t.Fatalf("first request: %s", first)
	}
	if out := stream.String(); !strings.Contains(out, `"adapter":"agent"`) || !strings.Contains(out, `"tool":"get_workload_status"`) {
		t.Fatalf("audit stream:\n%s", out)
	}
}
