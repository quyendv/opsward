package app

import (
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

// RBACOptions names who the rendered RBAC is bound to.
type RBACOptions struct {
	// ServiceAccountNamespace holds the default subjects, ServiceAccounts named
	// opsward-<profile>; default opsward.
	ServiceAccountNamespace string
	// Subjects replace the default subject of a profile.
	Subjects map[string]rbacv1.Subject
	// Cluster, if set, limits the output to one cluster: what is applied there, or
	// installed with the Helm chart running in it.
	Cluster string
}

// ParseRBACSubject reads <profile>=<User|Group|ServiceAccount>:<name>, where a
// ServiceAccount's name is <namespace>/<name>.
func ParseRBACSubject(s string) (string, rbacv1.Subject, error) {
	profile, rest, ok := strings.Cut(s, "=")
	kind, name, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || profile == "" || name == "" {
		return "", rbacv1.Subject{}, fmt.Errorf("subject %q: want <profile>=<User|Group|ServiceAccount>:<name>", s)
	}
	switch kind {
	case rbacv1.UserKind, rbacv1.GroupKind:
		return profile, rbacv1.Subject{Kind: kind, APIGroup: rbacv1.GroupName, Name: name}, nil
	case rbacv1.ServiceAccountKind:
		ns, n, found := strings.Cut(name, "/")
		if !found || ns == "" || n == "" {
			return "", rbacv1.Subject{}, fmt.Errorf("subject %q: a ServiceAccount is <namespace>/<name>", s)
		}
		return profile, rbacv1.Subject{Kind: kind, Namespace: ns, Name: n}, nil
	}
	return "", rbacv1.Subject{}, fmt.Errorf("subject %q: kind %q is not User, Group or ServiceAccount", s, kind)
}

// Object names are opsward:<profile>[:cluster|:delivery]. Profile names are DNS-1123
// labels, which have no colon, so no two profiles can render the same name.
const rbacPrefix = "opsward:"

// RenderRBAC prints, per cluster and profile, the Kubernetes RBAC that the tools policy
// lets run through the profile need (docs/design/0009). Nothing is applied.
func RenderRBAC(cfg *config.Config, opt RBACOptions) (string, error) {
	if opt.ServiceAccountNamespace == "" {
		opt.ServiceAccountNamespace = "opsward"
	}
	tools, err := checkTools(cfg)
	if err != nil {
		return "", err
	}
	needs := map[string][]string{}
	for _, t := range tools {
		needs[t.Name] = t.Needs
	}
	pol, _, err := policy.Compile(cfg.Policy, catalog(cfg, tools), nil)
	if err != nil {
		return "", err
	}
	grants := pol.Grants(catalog(cfg, tools))

	var b strings.Builder
	b.WriteString("# Rendered by opsward rbac render. Review it, then apply it with an administrator's\n" +
		"# credentials for each cluster; Opsward never applies it.\n")
	clusters := make([]string, 0, len(cfg.Providers.Kubernetes.Clusters))
	for name := range cfg.Providers.Kubernetes.Clusters {
		clusters = append(clusters, name)
	}
	slices.Sort(clusters)
	if opt.Cluster != "" {
		if _, ok := cfg.Providers.Kubernetes.Clusters[opt.Cluster]; !ok {
			return "", fmt.Errorf("--cluster: the configuration has no cluster %q", opt.Cluster)
		}
		clusters = []string{opt.Cluster}
	}
	bound := map[string]bool{}
	for _, cluster := range clusters {
		objs, err := clusterRBAC(cfg, cluster, grants, needs, pol.NeedsLabels(), opt, bound)
		if err != nil {
			return "", err
		}
		if len(objs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n# Cluster %s\n", cluster)
		for _, o := range objs {
			y, err := yaml.Marshal(o.obj)
			if err != nil {
				return "", err
			}
			if o.note != "" {
				b.WriteString("# " + o.note + "\n")
			}
			b.WriteString("---\n")
			b.Write(y)
		}
	}
	for profile := range opt.Subjects {
		if !bound[profile] {
			return "", fmt.Errorf("--subject names profile %q, which no rendered object binds", profile)
		}
	}
	return b.String(), nil
}

// profileUse is what one profile does on one cluster.
type profileUse struct {
	ops        []string
	namespaces []string // of the environments it serves
	// wide names the environments it serves that have namespace patterns, with them.
	wide []string
}

// object is one rendered object, with a note shown above it.
type object struct {
	obj  any
	note string
}

// clusterRBAC renders one cluster's objects, and records in bound the profiles it binds.
func clusterRBAC(cfg *config.Config, cluster string, grants map[string]map[string][]string, needs map[string][]string, labels bool, opt RBACOptions, bound map[string]bool) ([]object, error) {
	declared := cfg.Providers.Kubernetes.Clusters[cluster].Profiles
	uses := map[string]*profileUse{}
	use := func(profile string, env config.Environment, ops ...string) {
		u := uses[profile]
		if u == nil {
			u = &profileUse{}
			uses[profile] = u
		}
		for _, op := range ops {
			if !slices.Contains(u.ops, op) {
				u.ops = append(u.ops, op)
			}
		}
		var patterns []string
		for _, ns := range env.Namespaces {
			if strings.ContainsAny(ns, "*?[") {
				patterns = append(patterns, ns)
			} else if !slices.Contains(u.namespaces, ns) {
				u.namespaces = append(u.namespaces, ns)
			}
		}
		if w := fmt.Sprintf("environment %s (%s)", env.Name, strings.Join(patterns, ", ")); len(patterns) > 0 && !slices.Contains(u.wide, w) {
			u.wide = append(u.wide, w)
		}
	}
	for _, env := range cfg.Environments {
		if env.Cluster != cluster {
			continue
		}
		for profile, tools := range grants[env.Name] {
			for _, t := range tools {
				use(profile, env, needs[t]...)
				// The engine reads a targeted workload's labels with the reader before
				// authorization, whatever profile the call then runs with, when policy
				// looks at labels.
				if labels && slices.Contains(needs[t], capability.OpWorkloadsGet) {
					if _, ok := declared[core.ProfileReader]; ok {
						use(core.ProfileReader, env, capability.OpWorkloadsGet)
					}
				}
			}
		}
	}

	subject := func(profile string) rbacv1.Subject {
		bound[profile] = true
		if s, ok := opt.Subjects[profile]; ok {
			return s
		}
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: opt.ServiceAccountNamespace, Name: "opsward-" + profile}
	}
	var objs []object
	profiles := make([]string, 0, len(uses))
	for p := range uses {
		profiles = append(profiles, p)
	}
	slices.Sort(profiles)
	for _, profile := range profiles {
		if _, ok := declared[profile]; !ok {
			return nil, fmt.Errorf("cluster %s: policy uses profile %q, which the cluster does not declare", cluster, profile)
		}
		u := uses[profile]
		namespaced, clusterRules := kubernetes.Rules(u.ops)
		name := rbacPrefix + profile
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
		subjects := []rbacv1.Subject{subject(profile)}
		if len(namespaced) > 0 {
			objs = append(objs, object{obj: clusterRole(name, namespaced)})
			if len(u.wide) > 0 {
				slices.Sort(u.wide)
				objs = append(objs, object{
					obj:  clusterRoleBinding(name, ref, subjects),
					note: fmt.Sprintf("%s is bound cluster-wide by the namespace patterns of %s.", name, strings.Join(u.wide, ", ")),
				})
			} else {
				slices.Sort(u.namespaces)
				for _, ns := range u.namespaces {
					objs = append(objs, object{obj: roleBinding(name, ns, ref, subjects)})
				}
			}
		}
		if len(clusterRules) > 0 {
			cname := name + ":cluster"
			objs = append(objs, object{obj: clusterRole(cname, clusterRules)},
				object{obj: clusterRoleBinding(cname, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cname}, subjects)})
		}
	}
	if d := cfg.Providers.Delivery; d != nil && d.Cluster == cluster {
		rules, _ := kubernetes.Rules([]string{capability.OpApplicationsGet})
		name := rbacPrefix + d.Profile + ":delivery"
		for _, ns := range append([]string{d.Namespace}, d.ApplicationNamespaces...) {
			objs = append(objs,
				object{obj: &rbacv1.Role{TypeMeta: typeMeta("Role"), ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Rules: rules}},
				object{obj: roleBinding(name, ns, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}, []rbacv1.Subject{subject(d.Profile)})})
		}
	}
	return objs, unique(objs)
}

// unique refuses two objects of one kind and name in one namespace: kubectl apply would
// merge them into one, granting one subject the other's rules.
func unique(objs []object) error {
	seen := map[string]bool{}
	for _, o := range objs {
		var kind string
		var meta metav1.ObjectMeta
		switch v := o.obj.(type) {
		case *rbacv1.ClusterRole:
			kind, meta = v.Kind, v.ObjectMeta
		case *rbacv1.ClusterRoleBinding:
			kind, meta = v.Kind, v.ObjectMeta
		case *rbacv1.Role:
			kind, meta = v.Kind, v.ObjectMeta
		case *rbacv1.RoleBinding:
			kind, meta = v.Kind, v.ObjectMeta
		}
		key := kind + "/" + meta.Namespace + "/" + meta.Name
		if seen[key] {
			return fmt.Errorf("two %s objects would be named %s", kind, meta.Name)
		}
		seen[key] = true
	}
	return nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: kind}
}

func clusterRole(name string, rules []rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{TypeMeta: typeMeta("ClusterRole"), ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}
}

func clusterRoleBinding(name string, ref rbacv1.RoleRef, subjects []rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{TypeMeta: typeMeta("ClusterRoleBinding"), ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: ref, Subjects: subjects}
}

func roleBinding(name, ns string, ref rbacv1.RoleRef, subjects []rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{TypeMeta: typeMeta("RoleBinding"), ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, RoleRef: ref, Subjects: subjects}
}
