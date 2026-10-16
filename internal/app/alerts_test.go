package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	a, err := Open(context.Background(), cfg, Options{
		Stderr: &strings.Builder{}, ConfigPath: path,
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
}
