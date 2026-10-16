package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
)

// patches records patch actions: their options and body.
func patches(c *fake.Clientset) *[]k8stesting.PatchActionImpl {
	var out []k8stesting.PatchActionImpl
	c.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		out = append(out, a.(k8stesting.PatchActionImpl))
		return false, nil, nil
	})
	return &out
}

var apiRef = capability.WorkloadRef{Namespace: "shop", Kind: "Deployment", Name: "api"}

func TestScaleWorkload(t *testing.T) {
	d := deployment()
	d.ResourceVersion = "7" // what Get read: the patch fails if the object changed since
	c := fake.NewClientset(d)
	got := patches(c)
	w := NewWorkloads(c)
	ch, err := w.ScaleWorkload(context.Background(), apiRef, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Diff != "spec.replicas: 2 -> 5" || ch.Previous["spec.replicas"] != int32(2) {
		t.Fatalf("change = %+v", ch)
	}
	p := (*got)[0]
	if p.GetPatchType() != "application/merge-patch+json" || string(p.GetPatch()) != `{"metadata":{"resourceVersion":"7"},"spec":{"replicas":5}}` {
		t.Fatalf("patch = %s %s", p.GetPatchType(), p.GetPatch())
	}
	opts := p.PatchOptions
	if opts.FieldManager != "opsward" || len(opts.DryRun) != 1 || opts.DryRun[0] != metav1.DryRunAll {
		t.Fatalf("options = %+v", opts)
	}
	if _, err := w.ScaleWorkload(context.Background(), apiRef, 5, false); err != nil || len((*got)[1].PatchOptions.DryRun) != 0 {
		t.Fatalf("apply: %v, %+v", err, (*got)[1].PatchOptions)
	}
}

func TestScaleRefusals(t *testing.T) {
	managed := deployment()
	managed.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "app:apps/Deployment:shop/api"}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}
	for name, tt := range map[string]struct {
		c    *fake.Clientset
		ref  capability.WorkloadRef
		want string
	}{
		"reconciled": {fake.NewClientset(managed), apiRef, "Argo CD"},
		"autoscaled": {fake.NewClientset(deployment(), hpa), apiRef, "HorizontalPodAutoscaler"},
		"daemon set": {fake.NewClientset(), capability.WorkloadRef{Namespace: "shop", Kind: "DaemonSet", Name: "agent"}, "cannot be scaled"},
		"not found":  {fake.NewClientset(), apiRef, "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			got := patches(tt.c)
			_, err := NewWorkloads(tt.c).ScaleWorkload(context.Background(), tt.ref, 3, true)
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(*got) != 0 {
				t.Fatalf("err = %v, patches %d", err, len(*got))
			}
			if name != "not found" && !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("a refusal is invalid input: %v", err)
			}
		})
	}
}

func TestRestartWorkload(t *testing.T) {
	d := deployment()
	d.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	c := fake.NewClientset(d, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}})
	got := patches(c)
	w := NewWorkloads(c)
	ch, err := w.RestartWorkload(context.Background(), apiRef, false)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Reconciler != "Helm" || !strings.HasPrefix(ch.Diff, `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]: (none) -> `) {
		t.Fatalf("change = %+v", ch)
	}
	if !strings.Contains(string((*got)[0].GetPatch()), `"kubectl.kubernetes.io/restartedAt"`) {
		t.Fatalf("patch = %s", (*got)[0].GetPatch())
	}
	if _, err := w.RestartWorkload(context.Background(), capability.WorkloadRef{Namespace: "shop", Kind: "StatefulSet", Name: "db"}, true); err != nil {
		t.Fatal(err)
	}
}

// An approval is checked by comparing a new preview with the approved one, so a preview
// must not change by itself: a restart's diff does not carry the time.
func TestRestartPreviewIsStable(t *testing.T) {
	c := fake.NewClientset(deployment())
	// The fake applies dry runs; an API server does not.
	c.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return len(a.(k8stesting.PatchActionImpl).PatchOptions.DryRun) > 0, nil, nil
	})
	w := NewWorkloads(c)
	a, err := w.RestartWorkload(context.Background(), apiRef, true)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	b, _ := w.RestartWorkload(context.Background(), apiRef, true)
	if a.Diff != b.Diff {
		t.Fatalf("previews differ: %q vs %q", a.Diff, b.Diff)
	}
}

// Scaling is refused whenever something may revert it: when unsure, refuse.
func TestScaleRefusesEveryKnownReconciler(t *testing.T) {
	isController := true
	for name, tt := range map[string]struct {
		mutate func(*appsv1.Deployment)
		want   string
	}{
		"argo label tracking": {func(d *appsv1.Deployment) { d.Labels = map[string]string{"app.kubernetes.io/instance": "api"} }, "app.kubernetes.io/instance"},
		"helm annotations":    {func(d *appsv1.Deployment) { d.Annotations = map[string]string{"meta.helm.sh/release-name": "api"} }, "Helm"},
		"controller owner": {func(d *appsv1.Deployment) {
			d.OwnerReferences = []metav1.OwnerReference{{APIVersion: "example.com/v1", Kind: "App", Name: "api", Controller: &isController}}
		}, "App api"},
	} {
		t.Run(name, func(t *testing.T) {
			d := deployment()
			tt.mutate(d)
			c := fake.NewClientset(d)
			_, err := NewWorkloads(c).ScaleWorkload(context.Background(), apiRef, 3, true)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A restart that rolls no pod must not report success.
func TestRestartRefusesWhatWouldNotRoll(t *testing.T) {
	paused := deployment()
	paused.Spec.Paused = true
	onDelete := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       appsv1.StatefulSetSpec{UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop"},
		Spec:       appsv1.DaemonSetSpec{UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}},
	}
	c := fake.NewClientset(paused, onDelete, ds)
	got := patches(c)
	w := NewWorkloads(c)
	for _, tt := range []struct {
		ref  capability.WorkloadRef
		want string
	}{
		{apiRef, "paused"},
		{capability.WorkloadRef{Namespace: "shop", Kind: "StatefulSet", Name: "db"}, "OnDelete"},
		{capability.WorkloadRef{Namespace: "shop", Kind: "DaemonSet", Name: "agent"}, "OnDelete"},
	} {
		if _, err := w.RestartWorkload(context.Background(), tt.ref, true); err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("%s: err = %v", tt.ref.Kind, err)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("patched %d times", len(*got))
	}
}

// The diff shows what the API server would store, mutating webhooks included.
func TestScaleDiffComesFromTheServersAnswer(t *testing.T) {
	c := fake.NewClientset(deployment())
	c.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		d := deployment()
		d.Spec.Replicas = ptr(int32(6)) // a webhook rounded the request up
		return true, d, nil
	})
	ch, err := NewWorkloads(c).ScaleWorkload(context.Background(), apiRef, 5, true)
	if err != nil || ch.Diff != "spec.replicas: 2 -> 6" {
		t.Fatalf("change = %+v, %v", ch, err)
	}
}
