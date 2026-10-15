package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// Label lookup runs before authorization, so it must cost one GET and nothing else.
func TestWorkloadLabelsIsOneGet(t *testing.T) {
	c := fake.NewClientset(deployment(), pod("api-a", map[string]string{"app": "api"}, true, 0, ""))
	c.ClearActions()
	labels, err := NewWorkloads(c).WorkloadLabels(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"})
	if err != nil || labels["app.kubernetes.io/part-of"] != "checkout" {
		t.Fatalf("WorkloadLabels = %v, %v", labels, err)
	}
	if a := c.Actions(); len(a) != 1 || a[0].GetVerb() != "get" {
		t.Fatalf("actions = %v, want a single get", a)
	}
	_, err = NewWorkloads(c).WorkloadLabels(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "StatefulSet", Name: "api"})
	if !errors.Is(err, capability.ErrNotFound) {
		t.Fatalf("missing workload: err = %v", err)
	}
}

func ev(name string, uid types.UID, kind, objName, reason string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: objName, UID: uid},
		Type:           "Warning", Reason: reason, Message: reason + " happened", Count: 2,
		LastTimestamp: metav1.NewTime(at),
	}
}

// Events of the Deployment, of the ReplicaSets it controls and of its pods; nothing else.
func TestWorkloadEvents(t *testing.T) {
	d := deployment()
	d.UID = "d1"
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-7d9f", Namespace: "shop", UID: "r1", Labels: map[string]string{"app": "api"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "d1", Controller: ptr(true)}},
	}}
	p := pod("api-7d9f-a", map[string]string{"app": "api"}, false, 0, "")
	p.UID = "p1"
	other := pod("worker-a", map[string]string{"app": "worker"}, true, 0, "")
	other.UID = "x1"
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	c := fake.NewClientset(d, rs, p, other,
		ev("e1", "d1", "Deployment", "api", "ScalingReplicaSet", t0),
		ev("e2", "r1", "ReplicaSet", "api-7d9f", "FailedCreate", t0.Add(time.Minute)),
		ev("e3", "p1", "Pod", "api-7d9f-a", "FailedScheduling", t0.Add(2*time.Minute)),
		ev("e4", "x1", "Pod", "worker-a", "Unrelated", t0.Add(3*time.Minute)),
	)
	ref := capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}
	got, err := NewWorkloads(c).WorkloadEvents(context.Background(), ref, 50)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, e := range got {
		reasons = append(reasons, e.Object+":"+e.Reason)
	}
	if strings.Join(reasons, ",") != "Pod/api-7d9f-a:FailedScheduling,ReplicaSet/api-7d9f:FailedCreate,Deployment/api:ScalingReplicaSet" {
		t.Fatalf("events = %v", reasons)
	}
	if got[0].Count != 2 || got[0].Type != "Warning" || !got[0].Time.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("first event = %+v", got[0])
	}
	limited, _ := NewWorkloads(c).WorkloadEvents(context.Background(), ref, 1)
	if len(limited) != 1 || limited[0].Reason != "FailedScheduling" {
		t.Fatalf("limit 1 = %+v", limited)
	}
}

func twoContainerPod(name string) *corev1.Pod {
	p := pod(name, map[string]string{"app": "api"}, true, 0, "")
	p.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: "sidecar"}}
	return p
}

func TestWorkloadLogs(t *testing.T) {
	c := fake.NewClientset(deployment(), twoContainerPod("api-b"), twoContainerPod("api-a"))
	ref := capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}
	w := NewWorkloads(c)

	all, err := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{TailLines: 10, MaxPods: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range all.Logs {
		got = append(got, l.Pod+"/"+l.Container+"="+strings.Join(l.Lines, "|"))
	}
	// The fake clientset answers every log request with "fake logs".
	if strings.Join(got, ",") != "api-a/app=fake logs,api-a/sidecar=fake logs,api-b/app=fake logs,api-b/sidecar=fake logs" || all.PodsOmitted != 0 {
		t.Fatalf("logs = %v, omitted %d", got, all.PodsOmitted)
	}

	one, _ := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{Container: "app", TailLines: 10, MaxPods: 1})
	if len(one.Logs) != 1 || one.Logs[0].Pod != "api-a" || one.PodsOmitted != 1 {
		t.Fatalf("container app, max 1 pod = %+v", one)
	}

	missing, _ := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{Container: "nope", TailLines: 10, MaxPods: 10})
	if len(missing.Logs) != 2 || missing.Logs[0].Error == "" || len(missing.Logs[0].Lines) != 0 {
		t.Fatalf("missing container = %+v", missing)
	}
}

func TestSourceUpdateRebuildsOnlyChangedProfiles(t *testing.T) {
	var built []string
	src := NewSource(
		map[string]string{"staging": "cluster-a"},
		map[string]map[string]Profile{"cluster-a": {"reader": {Kubeconfig: "/r"}, "team": {Kubeconfig: "/t"}}},
		func(p Profile) (kubernetes.Interface, error) {
			built = append(built, p.Kubeconfig)
			return fake.NewClientset(), nil
		},
	)
	for _, pr := range []string{"reader", "team"} {
		if _, err := src.Workloads("staging", pr); err != nil {
			t.Fatal(err)
		}
	}
	src.Update(
		map[string]string{"production": "cluster-a"},
		map[string]map[string]Profile{"cluster-a": {"reader": {Kubeconfig: "/r2"}, "team": {Kubeconfig: "/t"}}},
	)
	if _, err := src.Workloads("staging", "reader"); err == nil {
		t.Fatal("a removed environment must be unknown after Update")
	}
	for _, pr := range []string{"reader", "team"} {
		if _, err := src.Workloads("production", pr); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(built, ",") != "/r,/t,/r2" {
		t.Fatalf("clients built = %v; want the changed reader rebuilt and team reused", built)
	}
}

// The kubelet's byte limit keeps the start of the tail window; Opsward keeps its end,
// where the crash is, cut at a line boundary.
func TestKeepTail(t *testing.T) {
	text, truncated := keepTail([]byte("line1\nline2\nline3\n"), 8)
	if text != "line3\n" || !truncated {
		t.Fatalf("keepTail = %q, %v", text, truncated)
	}
	text, truncated = keepTail([]byte("line1\nline2\n"), 100)
	if text != "line1\nline2\n" || truncated {
		t.Fatalf("keepTail under the cap = %q, %v", text, truncated)
	}
}

func TestWorkloadLogsTotalBudgetAndPodFilter(t *testing.T) {
	c := fake.NewClientset(deployment(), twoContainerPod("api-b"), twoContainerPod("api-a"))
	ref := capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}
	w := NewWorkloads(c)

	// "fake logs" is 9 bytes: two containers fill 18 of 20, the third crosses the budget,
	// the fourth is not read.
	got, err := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{TailLines: 10, MaxPods: 10, TotalBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Logs) != 3 || got.ContainersOmitted != 1 {
		t.Fatalf("budget: %d logs, %d omitted", len(got.Logs), got.ContainersOmitted)
	}

	one, err := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{Pod: "api-b", TailLines: 10, MaxPods: 10})
	if err != nil || len(one.Logs) != 2 || one.Logs[0].Pod != "api-b" {
		t.Fatalf("pod filter = %+v, %v", one, err)
	}
	if _, err := w.WorkloadLogs(context.Background(), ref, capability.LogOptions{Pod: "other", TailLines: 10, MaxPods: 10}); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("foreign pod: err = %v", err)
	}
}

func TestWorkloadLogsInitContainers(t *testing.T) {
	p := pod("api-a", map[string]string{"app": "api"}, false, 0, "")
	p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	p.Spec.Containers = []corev1.Container{{Name: "app"}}
	c := fake.NewClientset(deployment(), p)
	ref := capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}
	all, err := NewWorkloads(c).WorkloadLogs(context.Background(), ref, capability.LogOptions{TailLines: 10, MaxPods: 10})
	if err != nil || len(all.Logs) != 2 || all.Logs[0].Container != "migrate" {
		t.Fatalf("default containers = %+v, %v", all, err)
	}
	one, _ := NewWorkloads(c).WorkloadLogs(context.Background(), ref, capability.LogOptions{Container: "migrate", TailLines: 10, MaxPods: 10})
	if len(one.Logs) != 1 || one.Logs[0].Error != "" {
		t.Fatalf("init container by name = %+v", one)
	}
}

// events.k8s.io/v1 producers (the scheduler) set EventTime and Series, not the core
// timestamps and count.
func TestWorkloadEventsSeries(t *testing.T) {
	d := deployment()
	d.UID = "d1"
	first := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	e := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "api", UID: "d1"},
		Reason:         "FailedScheduling", Type: "Warning",
		EventTime: metav1.NewMicroTime(first),
		Series:    &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(first.Add(time.Hour))},
	}
	got, err := NewWorkloads(fake.NewClientset(d, e)).WorkloadEvents(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("events = %+v, %v", got, err)
	}
	if got[0].Count != 7 || !got[0].Time.Equal(first.Add(time.Hour)) {
		t.Fatalf("series event = %+v", got[0])
	}
}
