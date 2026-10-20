package kubernetes

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/quyendv/opsward/internal/capability"
)

// ListNamespaces implements capability.Workloads.
func (w *workloads) ListNamespaces(ctx context.Context) ([]string, error) {
	list, err := w.c.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, n.Name)
	}
	slices.Sort(out)
	return out, nil
}

// ListWorkloads implements capability.Workloads. A profile acting as its caller keeps
// the kinds the caller may list, and wraps ErrForbidden when it may list none; for a
// profile acting as itself a refusal is a misconfiguration, and fails.
func (w *workloads) ListWorkloads(ctx context.Context, namespace string) ([]capability.WorkloadSummary, error) {
	apps := w.c.AppsV1()
	var out []capability.WorkloadSummary
	var refused error
	listed := false
	check := func(err error) (bool, error) {
		if err == nil {
			listed = true
			return true, nil
		}
		if w.asCaller && apierrors.IsForbidden(err) {
			refused = err
			return false, nil
		}
		return false, err
	}
	deps, err := apps.Deployments(namespace).List(ctx, metav1.ListOptions{})
	if ok, err := check(err); err != nil {
		return nil, err
	} else if ok {
		for _, d := range deps.Items {
			out = append(out, capability.WorkloadSummary{Namespace: d.Namespace, Kind: "Deployment", Name: d.Name, Desired: replicasOrOne(d.Spec.Replicas), Ready: d.Status.ReadyReplicas, Labels: d.Labels})
		}
	}
	sts, err := apps.StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if ok, err := check(err); err != nil {
		return nil, err
	} else if ok {
		for _, s := range sts.Items {
			out = append(out, capability.WorkloadSummary{Namespace: s.Namespace, Kind: "StatefulSet", Name: s.Name, Desired: replicasOrOne(s.Spec.Replicas), Ready: s.Status.ReadyReplicas, Labels: s.Labels})
		}
	}
	dss, err := apps.DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if ok, err := check(err); err != nil {
		return nil, err
	} else if ok {
		for _, d := range dss.Items {
			out = append(out, capability.WorkloadSummary{Namespace: d.Namespace, Kind: "DaemonSet", Name: d.Name, Desired: d.Status.DesiredNumberScheduled, Ready: d.Status.NumberReady, Labels: d.Labels})
		}
	}
	if !listed {
		return nil, fmt.Errorf("%w: %w", capability.ErrForbidden, refused)
	}
	slices.SortFunc(out, func(a, b capability.WorkloadSummary) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// WorkloadRestarts implements capability.Workloads. A pod belongs to the workload that
// controls it, through its ReplicaSet for a Deployment.
func (w *workloads) WorkloadRestarts(ctx context.Context, namespace string) ([]capability.WorkloadRestarts, error) {
	pods, err := w.c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rss, err := w.c.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	type key struct{ namespace, name string }
	deploymentOf := map[key]string{}
	for _, rs := range rss.Items {
		if o := metav1.GetControllerOf(&rs); o != nil && o.Kind == "Deployment" {
			deploymentOf[key{rs.Namespace, rs.Name}] = o.Name
		}
	}
	byWorkload := map[capability.WorkloadRef]*capability.WorkloadRestarts{}
	for _, p := range pods.Items {
		o := metav1.GetControllerOf(&p)
		if o == nil {
			continue
		}
		ref := capability.WorkloadRef{Namespace: p.Namespace, Kind: o.Kind, Name: o.Name}
		switch o.Kind {
		case "ReplicaSet":
			d, ok := deploymentOf[key{p.Namespace, o.Name}]
			if !ok {
				continue
			}
			ref.Kind, ref.Name = "Deployment", d
		case "StatefulSet", "DaemonSet":
		default:
			continue
		}
		for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			if cs.RestartCount == 0 {
				continue
			}
			r := byWorkload[ref]
			if r == nil {
				r = &capability.WorkloadRestarts{Namespace: ref.Namespace, Kind: ref.Kind, Name: ref.Name}
				byWorkload[ref] = r
			}
			r.Restarts += cs.RestartCount
			if t := cs.LastTerminationState.Terminated; t != nil && !t.FinishedAt.IsZero() && (r.LastRestart == nil || t.FinishedAt.After(*r.LastRestart)) {
				last := t.FinishedAt.UTC()
				r.LastRestart = &last
			}
		}
	}
	out := make([]capability.WorkloadRestarts, 0, len(byWorkload))
	for _, r := range byWorkload {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b capability.WorkloadRestarts) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}
