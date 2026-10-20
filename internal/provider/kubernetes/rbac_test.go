package kubernetes

import (
	"context"
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/quyendv/opsward/internal/capability"
)

var errNotScalable = errors.New("not scalable")

// allows reports whether one of the rules permits the action.
func allows(rules []rbacv1.PolicyRule, a k8stesting.Action) bool {
	res := a.GetResource().Resource
	if a.GetSubresource() != "" {
		res += "/" + a.GetSubresource()
	}
	for _, r := range rules {
		if slices.Contains(r.APIGroups, a.GetResource().Group) && slices.Contains(r.Resources, res) && slices.Contains(r.Verbs, a.GetVerb()) {
			return true
		}
	}
	return false
}

// Every request a Workloads method makes is allowed by the rules of the operations
// capability.WorkloadsOps gives it (the table the tool package checks tools against), so
// the rendered RBAC is enough. docs/design/0009.
func TestOperationRulesCoverTheRequests(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop"},
		Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}}},
	}
	refs := []capability.WorkloadRef{apiRef, {Namespace: "shop", Kind: "StatefulSet", Name: "db"}, {Namespace: "shop", Kind: "DaemonSet", Name: "agent"}}
	ctx := context.Background()
	type opCase struct {
		name string
		call func(capability.Workloads, capability.WorkloadRef) error
	}
	tests := []opCase{
		{"GetWorkload", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.GetWorkload(ctx, r)
			return err
		}},
		{"WorkloadLabels", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.WorkloadLabels(ctx, r)
			return err
		}},
		{"WorkloadMetadata", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.WorkloadMetadata(ctx, r)
			return err
		}},
		{"WorkloadEvents", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.WorkloadEvents(ctx, r, 10)
			return err
		}},
		{"WorkloadLogs", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.WorkloadLogs(ctx, r, capability.LogOptions{TailLines: 10, MaxPods: 2, MaxBytes: 1000, TotalBytes: 2000})
			return err
		}},
		{"PodNamePattern", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.PodNamePattern(ctx, r)
			return err
		}},
		{"ListNamespaces", func(w capability.Workloads, _ capability.WorkloadRef) error {
			_, err := w.ListNamespaces(ctx)
			return err
		}},
		{"ListWorkloads", func(w capability.Workloads, _ capability.WorkloadRef) error {
			if _, err := w.ListWorkloads(ctx, ""); err != nil {
				return err
			}
			_, err := w.ListWorkloads(ctx, "shop")
			return err
		}},
		{"WorkloadRestarts", func(w capability.Workloads, _ capability.WorkloadRef) error {
			if _, err := w.WorkloadRestarts(ctx, ""); err != nil {
				return err
			}
			_, err := w.WorkloadRestarts(ctx, "shop")
			return err
		}},
		{"RestartWorkload", func(w capability.Workloads, r capability.WorkloadRef) error {
			_, err := w.RestartWorkload(ctx, r, true)
			return err
		}},
		{"ScaleWorkload", func(w capability.Workloads, r capability.WorkloadRef) error {
			if r.Kind == "DaemonSet" {
				return errNotScalable
			}
			_, err := w.ScaleWorkload(ctx, r, 3, true)
			return err
		}},
	}
	for method := range capability.WorkloadsOps {
		if !slices.ContainsFunc(tests, func(tt opCase) bool { return tt.name == method }) {
			t.Errorf("%s is not exercised", method)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, ok := capability.WorkloadsOps[tt.name]
			if !ok {
				t.Fatalf("%s is not in capability.WorkloadsOps", tt.name)
			}
			namespaced, cluster := Rules(ops)
			for _, r := range refs {
				d := deployment()
				d.UID = "d1"
				rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
					Name: "api-7d9f", Namespace: "shop", UID: "r1", Labels: map[string]string{"app": "api"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "d1", Controller: ptr(true)}},
				}}
				c := fake.NewClientset(d, rs, sts, ds, pod("api-7d9f-a", map[string]string{"app": "api"}, true, 0, ""))
				c.ClearActions()
				// An error (a Deployment without ReplicaSets) still made its requests.
				err := tt.call(NewWorkloads(c), r)
				if errors.Is(err, errNotScalable) {
					continue
				}
				if err != nil {
					t.Logf("%s: %v", r.Kind, err)
				}
				if len(c.Actions()) == 0 {
					t.Fatalf("%s: no request was made", r.Kind)
				}
				for _, a := range c.Actions() {
					rules := namespaced
					if a.GetResource().Resource == "namespaces" {
						rules = cluster
					}
					if !allows(rules, a) {
						t.Errorf("%s: %s %s/%s %q is not allowed by the rules of %v",
							r.Kind, a.GetVerb(), a.GetResource().Resource, a.GetSubresource(), a.GetNamespace(), ops)
					}
				}
			}
		})
	}
}

func TestRulesMergeAndSort(t *testing.T) {
	namespaced, cluster := Rules([]string{capability.OpWorkloadsList, capability.OpWorkloadsGet, capability.OpWorkloadsGet})
	if len(cluster) != 0 || len(namespaced) != 1 ||
		!slices.Equal(namespaced[0].Verbs, []string{"get", "list"}) ||
		!slices.Equal(namespaced[0].Resources, []string{"daemonsets", "deployments", "statefulsets"}) {
		t.Fatalf("rules = %+v / %+v", namespaced, cluster)
	}
}
