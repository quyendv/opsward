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
