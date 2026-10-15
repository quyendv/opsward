// Package kubernetes implements capabilities on the Kubernetes API.
package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func (w *workloads) WorkloadLabels(ctx context.Context, ref capability.WorkloadRef) (map[string]string, error) {
	meta, _, err := w.object(ctx, ref)
	if err != nil {
		return nil, err
	}
	return meta.GetLabels(), nil
}

// object reads the workload once and returns its metadata and pod selector.
func (w *workloads) object(ctx context.Context, ref capability.WorkloadRef) (metav1.Object, *metav1.LabelSelector, error) {
	var meta metav1.Object
	var sel *metav1.LabelSelector
	var err error
	switch ref.Kind {
	case "Deployment":
		var d *appsv1.Deployment
		if d, err = w.c.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, sel = d, d.Spec.Selector
		}
	case "StatefulSet":
		var s *appsv1.StatefulSet
		if s, err = w.c.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, sel = s, s.Spec.Selector
		}
	case "DaemonSet":
		var d *appsv1.DaemonSet
		if d, err = w.c.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); err == nil {
			meta, sel = d, d.Spec.Selector
		}
	default:
		return nil, nil, fmt.Errorf("unsupported workload kind %q", ref.Kind)
	}
	if apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, capability.ErrNotFound)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return meta, sel, nil
}

func (w *workloads) WorkloadEvents(ctx context.Context, ref capability.WorkloadRef, limit int) ([]capability.Event, error) {
	meta, sel, err := w.object(ctx, ref)
	if err != nil {
		return nil, err
	}
	uids := map[types.UID]bool{meta.GetUID(): true}
	if ref.Kind == "Deployment" {
		// FailedCreate (quota, admission) is reported on the ReplicaSet.
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("workload selector: %w", err)
		}
		rss, err := w.c.AppsV1().ReplicaSets(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: s.String()})
		if err != nil {
			return nil, fmt.Errorf("list replicasets: %w", err)
		}
		for i := range rss.Items {
			if metav1.IsControlledBy(&rss.Items[i], meta) {
				uids[rss.Items[i].UID] = true
			}
		}
	}
	pods, err := w.listPods(ctx, ref.Namespace, sel)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		uids[pods[i].UID] = true
	}
	list, err := w.c.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	out := []capability.Event{}
	for i := range list.Items {
		e := &list.Items[i]
		if !uids[e.InvolvedObject.UID] {
			continue
		}
		out = append(out, capability.Event{
			Time: eventTime(e), Type: e.Type, Reason: e.Reason,
			Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Message: e.Message, Count: e.Count,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.UTC()
	case !e.EventTime.IsZero():
		return e.EventTime.UTC()
	default:
		return e.FirstTimestamp.UTC()
	}
}

func (w *workloads) WorkloadLogs(ctx context.Context, ref capability.WorkloadRef, opt capability.LogOptions) (*capability.WorkloadLogs, error) {
	_, sel, err := w.object(ctx, ref)
	if err != nil {
		return nil, err
	}
	pods, err := w.listPods(ctx, ref.Namespace, sel)
	if err != nil {
		return nil, err
	}
	out := &capability.WorkloadLogs{Logs: []capability.ContainerLog{}}
	if opt.MaxPods > 0 && len(pods) > opt.MaxPods {
		out.PodsOmitted = len(pods) - opt.MaxPods
		pods = pods[:opt.MaxPods]
	}
	for i := range pods {
		p := &pods[i]
		names := []string{opt.Container}
		if opt.Container == "" {
			names = names[:0]
			for _, c := range p.Spec.Containers {
				names = append(names, c.Name)
			}
		}
		for _, name := range names {
			out.Logs = append(out.Logs, w.containerLog(ctx, p, name, opt))
		}
	}
	return out, nil
}

func (w *workloads) containerLog(ctx context.Context, p *corev1.Pod, container string, opt capability.LogOptions) capability.ContainerLog {
	entry := capability.ContainerLog{Pod: p.Name, Container: container}
	found := false
	for _, c := range p.Spec.Containers {
		found = found || c.Name == container
	}
	if !found {
		entry.Error = fmt.Sprintf("pod %s has no container %q", p.Name, container)
		return entry
	}
	po := &corev1.PodLogOptions{Container: container, Previous: opt.Previous, Timestamps: true}
	if opt.TailLines > 0 {
		po.TailLines = &opt.TailLines
	}
	if opt.LimitBytes > 0 {
		po.LimitBytes = &opt.LimitBytes
	}
	raw, err := w.c.CoreV1().Pods(p.Namespace).GetLogs(p.Name, po).DoRaw(ctx)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	if text := strings.TrimRight(string(raw), "\n"); text != "" {
		entry.Lines = strings.Split(text, "\n")
	}
	return entry
}

func (w *workloads) pods(ctx context.Context, namespace string, selector *metav1.LabelSelector) ([]capability.Pod, error) {
	list, err := w.listPods(ctx, namespace, selector)
	if err != nil {
		return nil, err
	}
	out := make([]capability.Pod, 0, len(list))
	for i := range list {
		out = append(out, podStatus(&list[i]))
	}
	return out, nil
}

// listPods returns the pods a selector matches, sorted by name.
func (w *workloads) listPods(ctx context.Context, namespace string, selector *metav1.LabelSelector) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("workload selector: %w", err)
	}
	// An empty selector would match every pod in the namespace.
	if sel.Empty() {
		return nil, nil
	}
	list, err := w.c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
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
