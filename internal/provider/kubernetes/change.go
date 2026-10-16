package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
)

const (
	fieldManager      = "opsward"
	restartAnnotation = "kubectl.kubernetes.io/restartedAt"
)

// reconciler names what manages an object, from the markers in docs/design/0007. When a
// marker is ambiguous it is still reported: refusing a change that would not have been
// reverted costs less than accepting one that will be.
func reconciler(m metav1.Object) string {
	a, l := m.GetAnnotations(), m.GetLabels()
	for _, o := range m.GetOwnerReferences() {
		if o.Controller != nil && *o.Controller {
			return fmt.Sprintf("its controller %s %s", o.Kind, o.Name)
		}
	}
	switch {
	case a["argocd.argoproj.io/tracking-id"] != "" || l["argocd.argoproj.io/instance"] != "":
		return "Argo CD"
	case l["kustomize.toolkit.fluxcd.io/name"] != "" || l["helm.toolkit.fluxcd.io/name"] != "":
		return "Flux"
	case l["app.kubernetes.io/managed-by"] == "Helm" || a["meta.helm.sh/release-name"] != "":
		return "Helm"
	case l["app.kubernetes.io/instance"] != "":
		// Argo CD's label tracking, or a chart's convention: either may revert it.
		return "the tool that set app.kubernetes.io/instance (Argo CD label tracking, Helm)"
	}
	return ""
}

// withPrecondition adds the resourceVersion that was read to a merge patch: the API server
// refuses it with a conflict if the object changed since, so the previous values recorded
// are the ones replaced.
func withPrecondition(m metav1.Object, patch map[string]any) map[string]any {
	if rv := m.GetResourceVersion(); rv != "" {
		patch["metadata"] = map[string]any{"resourceVersion": rv}
	}
	return patch
}

func patchOptions(dryRun bool) metav1.PatchOptions {
	o := metav1.PatchOptions{FieldManager: fieldManager}
	if dryRun {
		o.DryRun = []string{metav1.DryRunAll}
	}
	return o
}

func notFound(ref capability.WorkloadRef, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, capability.ErrNotFound)
	}
	return err
}

// RestartWorkload implements capability.Workloads.
func (w *workloads) RestartWorkload(ctx context.Context, ref capability.WorkloadRef, dryRun bool) (*capability.WorkloadChange, error) {
	var meta metav1.Object
	var annotations map[string]string
	var still string // why setting the annotation would roll no pod
	apps := w.c.AppsV1()
	var err error
	switch ref.Kind {
	case "Deployment":
		var d *appsv1.Deployment
		if d, err = apps.Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, annotations = d, d.Spec.Template.Annotations
			if d.Spec.Paused {
				still = "its rollout is paused"
			}
		}
	case "StatefulSet":
		var s *appsv1.StatefulSet
		if s, err = apps.StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, annotations = s, s.Spec.Template.Annotations
			if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
				still = "its update strategy is OnDelete: pods change only when deleted"
			}
		}
	case "DaemonSet":
		var d *appsv1.DaemonSet
		if d, err = apps.DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, annotations = d, d.Spec.Template.Annotations
			if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
				still = "its update strategy is OnDelete: pods change only when deleted"
			}
		}
	default:
		return nil, core.InvalidInput(fmt.Sprintf("kind %q cannot be restarted", ref.Kind))
	}
	if err != nil {
		return nil, notFound(ref, err)
	}
	if still != "" {
		return nil, core.InvalidInput(fmt.Sprintf("restarting %s %s/%s would roll no pod: %s", ref.Kind, ref.Namespace, ref.Name, still))
	}
	now := time.Now().UTC().Format(time.RFC3339)
	patch, err := json.Marshal(withPrecondition(meta, map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{restartAnnotation: now},
	}}}}))
	if err != nil {
		return nil, err
	}
	opts := patchOptions(dryRun)
	switch ref.Kind {
	case "Deployment":
		_, err = apps.Deployments(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, opts)
	case "StatefulSet":
		_, err = apps.StatefulSets(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, opts)
	case "DaemonSet":
		_, err = apps.DaemonSets(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, opts)
	}
	if err != nil {
		return nil, notFound(ref, err)
	}
	prev, ok := annotations[restartAnnotation]
	shown := prev
	if !ok {
		shown = "(none)"
	}
	field := fmt.Sprintf("spec.template.metadata.annotations[%q]", restartAnnotation)
	var previous any
	if ok {
		previous = prev
	}
	return &capability.WorkloadChange{
		// Not the time itself: a preview must compare equal to the one approved.
		Diff:       fmt.Sprintf("%s: %s -> (the time of the restart)", field, shown),
		Previous:   map[string]any{field: previous},
		Reconciler: reconciler(meta),
	}, nil
}

// ScaleWorkload implements capability.Workloads.
func (w *workloads) ScaleWorkload(ctx context.Context, ref capability.WorkloadRef, replicas int32, dryRun bool) (*capability.WorkloadChange, error) {
	apps := w.c.AppsV1()
	var meta metav1.Object
	var current int32
	var err error
	switch ref.Kind {
	case "Deployment":
		var d *appsv1.Deployment
		if d, err = apps.Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, current = d, replicasOrOne(d.Spec.Replicas)
		}
	case "StatefulSet":
		var s *appsv1.StatefulSet
		if s, err = apps.StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, current = s, replicasOrOne(s.Spec.Replicas)
		}
	default:
		return nil, core.InvalidInput(fmt.Sprintf("a %s cannot be scaled: it runs one pod per node", ref.Kind))
	}
	if err != nil {
		return nil, notFound(ref, err)
	}
	if r := reconciler(meta); r != "" {
		return nil, core.InvalidInput(fmt.Sprintf("%s manages %s %s/%s: its replicas are declared in the source, and %s would revert this change",
			r, ref.Kind, ref.Namespace, ref.Name, r))
	}
	hpas, err := w.c.AutoscalingV2().HorizontalPodAutoscalers(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading autoscalers: %w", err)
	}
	for _, h := range hpas.Items {
		if h.Spec.ScaleTargetRef.Kind == ref.Kind && h.Spec.ScaleTargetRef.Name == ref.Name {
			return nil, core.InvalidInput(fmt.Sprintf("HorizontalPodAutoscaler %s targets %s %s/%s and would undo this change",
				h.Name, ref.Kind, ref.Namespace, ref.Name))
		}
	}
	patch, err := json.Marshal(withPrecondition(meta, map[string]any{"spec": map[string]any{"replicas": replicas}}))
	if err != nil {
		return nil, err
	}
	opts := patchOptions(dryRun)
	// What the API server stores, or would store, admission included.
	after := replicas
	if ref.Kind == "Deployment" {
		var d *appsv1.Deployment
		if d, err = apps.Deployments(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, opts); err == nil && d != nil && d.Spec.Replicas != nil {
			after = *d.Spec.Replicas
		}
	} else {
		var s *appsv1.StatefulSet
		if s, err = apps.StatefulSets(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, opts); err == nil && s != nil && s.Spec.Replicas != nil {
			after = *s.Spec.Replicas
		}
	}
	if err != nil {
		return nil, notFound(ref, err)
	}
	return &capability.WorkloadChange{
		Diff:     fmt.Sprintf("spec.replicas: %d -> %d", current, after),
		Previous: map[string]any{"spec.replicas": current},
	}, nil
}
