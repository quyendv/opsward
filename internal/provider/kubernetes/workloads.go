// Package kubernetes implements capabilities on the Kubernetes API.
package kubernetes

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/quyendv/opsward/internal/capability"
)

type workloads struct {
	c kubernetes.Interface
}

// NewWorkloads returns capability.Workloads backed by c.
func NewWorkloads(c kubernetes.Interface) capability.Workloads {
	return &workloads{c: c}
}

func (w *workloads) GetWorkload(ctx context.Context, ref capability.WorkloadRef) (*capability.Workload, error) {
	out := &capability.Workload{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
	var selector *metav1.LabelSelector
	var err error

	switch ref.Kind {
	case "Deployment":
		var d *appsv1.Deployment
		d, err = w.c.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			selector, out.Labels = d.Spec.Selector, d.Labels
			out.Replicas = capability.Replicas{
				Desired: replicasOrOne(d.Spec.Replicas), Ready: d.Status.ReadyReplicas,
				Updated: d.Status.UpdatedReplicas, Available: d.Status.AvailableReplicas,
			}
			for _, c := range d.Status.Conditions {
				out.Conditions = append(out.Conditions, capability.Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
			}
		}
	case "StatefulSet":
		var s *appsv1.StatefulSet
		s, err = w.c.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			selector, out.Labels = s.Spec.Selector, s.Labels
			out.Replicas = capability.Replicas{
				Desired: replicasOrOne(s.Spec.Replicas), Ready: s.Status.ReadyReplicas,
				Updated: s.Status.UpdatedReplicas, Available: s.Status.AvailableReplicas,
			}
			for _, c := range s.Status.Conditions {
				out.Conditions = append(out.Conditions, capability.Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
			}
		}
	case "DaemonSet":
		var d *appsv1.DaemonSet
		d, err = w.c.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			selector, out.Labels = d.Spec.Selector, d.Labels
			out.Replicas = capability.Replicas{
				Desired: d.Status.DesiredNumberScheduled, Ready: d.Status.NumberReady,
				Updated: d.Status.UpdatedNumberScheduled, Available: d.Status.NumberAvailable,
			}
			for _, c := range d.Status.Conditions {
				out.Conditions = append(out.Conditions, capability.Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
			}
		}
	default:
		return nil, fmt.Errorf("unsupported workload kind %q", ref.Kind)
	}
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, capability.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}

	pods, err := w.pods(ctx, ref.Namespace, selector)
	if err != nil {
		return nil, err
	}
	out.Pods = pods
	return out, nil
}

func (w *workloads) pods(ctx context.Context, namespace string, selector *metav1.LabelSelector) ([]capability.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("workload selector: %w", err)
	}
	// An empty selector would match every pod in the namespace.
	if sel.Empty() {
		return []capability.Pod{}, nil
	}
	list, err := w.c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make([]capability.Pod, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, podStatus(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func podStatus(p *corev1.Pod) capability.Pod {
	out := capability.Pod{Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			out.Ready = c.Status == corev1.ConditionTrue
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		out.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && out.Waiting == "" {
			out.Waiting = cs.State.Waiting.Reason
		}
	}
	return out
}

func replicasOrOne(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
