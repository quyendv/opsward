package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quyendv/opsward/internal/capability"
)

func ptr[T any](v T) *T { return &v }

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/part-of": "checkout"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1, UpdatedReplicas: 2, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable"}},
		},
	}
}

func pod(name string, labels map[string]string, ready bool, restarts int32, waiting string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: restarts}},
		},
	}
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	if waiting != "" {
		p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting}
	}
	return p
}

func TestGetDeployment(t *testing.T) {
	c := fake.NewClientset(
		deployment(),
		pod("api-b", map[string]string{"app": "api"}, false, 7, "CrashLoopBackOff"),
		pod("api-a", map[string]string{"app": "api"}, true, 0, ""),
		pod("worker-a", map[string]string{"app": "worker"}, true, 0, ""),
	)
	w, err := NewWorkloads(c).GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"})
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if w.Replicas != (capability.Replicas{Desired: 2, Ready: 1, Updated: 2, Available: 1}) {
		t.Errorf("replicas = %+v", w.Replicas)
	}
	if w.Labels["app.kubernetes.io/part-of"] != "checkout" {
		t.Errorf("labels = %v", w.Labels)
	}
	if len(w.Conditions) != 1 || w.Conditions[0].Reason != "MinimumReplicasUnavailable" {
		t.Errorf("conditions = %+v", w.Conditions)
	}
	if len(w.Pods) != 2 || w.Pods[0].Name != "api-a" || w.Pods[1].Name != "api-b" {
		t.Fatalf("pods = %+v, want api-a and api-b only, sorted", w.Pods)
	}
	if b := w.Pods[1]; b.Ready || b.Restarts != 7 || b.Waiting != "CrashLoopBackOff" || b.Node != "node-1" {
		t.Errorf("api-b = %+v", b)
	}
}

func TestGetStatefulSetAndDaemonSet(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}
	c := fake.NewClientset(
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
			Spec:       appsv1.StatefulSetSpec{Selector: sel},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop"},
			Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}}},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberReady: 2, UpdatedNumberScheduled: 3, NumberAvailable: 2},
		},
	)
	w := NewWorkloads(c)
	sts, err := w.GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "StatefulSet", Name: "db"})
	if err != nil || sts.Replicas.Desired != 1 || sts.Replicas.Ready != 1 {
		t.Fatalf("StatefulSet = %+v, %v (nil spec.replicas means 1)", sts, err)
	}
	ds, err := w.GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "DaemonSet", Name: "agent"})
	if err != nil || ds.Replicas != (capability.Replicas{Desired: 3, Ready: 2, Updated: 3, Available: 2}) {
		t.Fatalf("DaemonSet = %+v, %v", ds, err)
	}
}

func TestGetWorkloadNotFoundAndUnsupportedKind(t *testing.T) {
	w := NewWorkloads(fake.NewClientset())
	_, err := w.GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "nope"})
	if !errors.Is(err, capability.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	_, err = w.GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Pod", Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "unsupported workload kind") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptySelectorListsNoPods(t *testing.T) {
	d := deployment()
	d.Spec.Selector = &metav1.LabelSelector{}
	c := fake.NewClientset(d, pod("unrelated", map[string]string{"x": "y"}, true, 0, ""))
	w, err := NewWorkloads(c).GetWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"})
	if err != nil || len(w.Pods) != 0 {
		t.Fatalf("pods = %+v, %v; an empty selector must not list the whole namespace", w, err)
	}
}

func TestSourceBuildsOneClientPerProfileAndFailsClosed(t *testing.T) {
	var built []Profile
	src := NewSource(
		map[string]string{"staging": "cluster-a"},
		map[string]map[string]Profile{"cluster-a": {"reader": {Kubeconfig: "/r"}, "team-a": {Kubeconfig: "/t"}}},
		func(p Profile) (kubernetes.Interface, error) {
			built = append(built, p)
			return fake.NewClientset(), nil
		},
	)
	for range 2 {
		if _, err := src.Workloads("staging", "reader"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.Workloads("staging", "team-a"); err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d clients, want one per profile", len(built))
	}
	if _, err := src.Workloads("staging", "operator"); err == nil {
		t.Fatal("an undeclared profile must be an error, not a fallback")
	}
	if _, err := src.Workloads("production", "reader"); err == nil {
		t.Fatal("an unknown environment must be an error")
	}
}

func TestFromKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kc := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:6443"}}]
users: [{name: u, user: {token: placeholder}}]
contexts: [{name: ctx, context: {cluster: c, user: u}}]
current-context: ctx
`
	if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FromKubeconfig(Profile{Kubeconfig: path}); err != nil {
		t.Fatalf("FromKubeconfig: %v", err)
	}
	if _, err := FromKubeconfig(Profile{Kubeconfig: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("a missing kubeconfig must be an error")
	}
}

// client-go's deferred loader falls back to the pod's own ServiceAccount when a
// kubeconfig is empty; a profile must never borrow another credential, so an empty or
// contextless kubeconfig is an error that names the file.
func TestFromKubeconfigRejectsEmptyAndContextless(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := FromKubeconfig(Profile{Kubeconfig: empty})
	if err == nil || !strings.Contains(err.Error(), empty) || !strings.Contains(err.Error(), "no contexts") {
		t.Fatalf("empty kubeconfig: err = %v", err)
	}

	noCurrent := filepath.Join(dir, "no-current")
	kc := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:6443"}}]
users: [{name: u, user: {token: placeholder}}]
contexts: [{name: ctx, context: {cluster: c, user: u}}]
`
	if err := os.WriteFile(noCurrent, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = FromKubeconfig(Profile{Kubeconfig: noCurrent})
	if err == nil || !strings.Contains(err.Error(), "no current-context") {
		t.Fatalf("no current-context: err = %v", err)
	}
	if _, err := FromKubeconfig(Profile{Kubeconfig: noCurrent, Context: "ctx"}); err != nil {
		t.Fatalf("explicit context: %v", err)
	}
	if _, err := FromKubeconfig(Profile{Kubeconfig: noCurrent, Context: "missing"}); err == nil {
		t.Fatal("a context the file does not define must be an error")
	}
}
