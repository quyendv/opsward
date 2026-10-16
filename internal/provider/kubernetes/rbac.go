package kubernetes

import (
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/quyendv/opsward/internal/capability"
)

type grant struct {
	group     string
	resources []string
	verbs     []string
	cluster   bool // a cluster-scoped resource
}

var workloadKinds = []string{"deployments", "statefulsets", "daemonsets"}

// opGrants are the permissions each operation needs, as this provider performs it.
// TestOperationRulesCoverTheRequests checks them against the requests made.
var opGrants = map[string][]grant{
	capability.OpWorkloadsGet: {{group: "apps", resources: workloadKinds, verbs: []string{"get"}}},
	capability.OpWorkloadsPods: {
		{group: "", resources: []string{"pods"}, verbs: []string{"list"}},
		{group: "apps", resources: []string{"replicasets"}, verbs: []string{"list"}},
	},
	capability.OpWorkloadsEvents:  {{group: "", resources: []string{"events"}, verbs: []string{"list"}}},
	capability.OpWorkloadsLogs:    {{group: "", resources: []string{"pods/log"}, verbs: []string{"get"}}},
	capability.OpWorkloadsList:    {{group: "apps", resources: workloadKinds, verbs: []string{"list"}}},
	capability.OpNamespacesList:   {{group: "", resources: []string{"namespaces"}, verbs: []string{"list"}, cluster: true}},
	capability.OpWorkloadsRestart: {{group: "apps", resources: workloadKinds, verbs: []string{"patch"}}},
	capability.OpWorkloadsScale: {
		{group: "apps", resources: []string{"deployments", "statefulsets"}, verbs: []string{"patch"}},
		{group: "autoscaling", resources: []string{"horizontalpodautoscalers"}, verbs: []string{"list"}},
	},
	capability.OpApplicationsGet: {{group: "argoproj.io", resources: []string{"applications"}, verbs: []string{"get"}}},
}

// Rules returns the RBAC rules the operations need: on namespaced resources and on
// cluster-scoped ones. Resources of one API group with the same verbs share a rule, and
// the output is sorted, so the same operations always render the same way. Unknown
// operations add nothing.
func Rules(ops []string) (namespaced, cluster []rbacv1.PolicyRule) {
	type key struct {
		group, resource string
		cluster         bool
	}
	verbs := map[key][]string{}
	for _, op := range ops {
		for _, g := range opGrants[op] {
			for _, r := range g.resources {
				k := key{g.group, r, g.cluster}
				for _, v := range g.verbs {
					if !slices.Contains(verbs[k], v) {
						verbs[k] = append(verbs[k], v)
					}
				}
			}
		}
	}
	type ruleKey struct {
		group, verbs string
		cluster      bool
	}
	byRule := map[ruleKey][]string{}
	for k, vs := range verbs {
		slices.Sort(vs)
		rk := ruleKey{k.group, strings.Join(vs, ","), k.cluster}
		byRule[rk] = append(byRule[rk], k.resource)
	}
	for rk, resources := range byRule {
		slices.Sort(resources)
		r := rbacv1.PolicyRule{APIGroups: []string{rk.group}, Resources: resources, Verbs: strings.Split(rk.verbs, ",")}
		if rk.cluster {
			cluster = append(cluster, r)
		} else {
			namespaced = append(namespaced, r)
		}
	}
	order := func(a, b rbacv1.PolicyRule) int {
		if c := strings.Compare(a.APIGroups[0], b.APIGroups[0]); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.Resources, ","), strings.Join(b.Resources, ","))
	}
	slices.SortFunc(namespaced, order)
	slices.SortFunc(cluster, order)
	return namespaced, cluster
}
