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
	gs := grantees(cfg, pol, catalog(cfg, tools))

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
		objs, err := clusterRBAC(cfg, cluster, grants, gs[cluster], needs, pol.NeedsLabels(), opt, bound)
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
func clusterRBAC(cfg *config.Config, cluster string, grants map[string]map[string][]string, reach map[string]policy.Grantees, needs map[string][]string, labels bool, opt RBACOptions, bound map[string]bool) ([]object, error) {
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
		if declared[profile].Impersonate != nil {
			// The cluster decides per person: whom to grant these rules is the operator's
			// decision about people, not one Opsward derives (docs/design/0014).
			slices.Sort(u.namespaces)
			slices.Sort(u.wide)
			where := "in namespaces " + strings.Join(u.namespaces, ", ")
			if len(u.wide) > 0 {
				where = "cluster-wide, for the namespace patterns of " + strings.Join(u.wide, ", ")
			}
			if len(namespaced) > 0 {
				objs = append(objs, object{obj: clusterRole(name, namespaced), note: fmt.Sprintf(
					"%s is not bound: profile %s acts as its callers, so bind it to the users and groups that need it, %s.", name, profile, where)})
			}
			if len(clusterRules) > 0 {
				objs = append(objs, object{obj: clusterRole(name+":cluster", clusterRules), note: fmt.Sprintf(
					"%s:cluster is not bound: bind it cluster-wide to the users and groups profile %s acts as.", name, profile)})
			}
			continue
		}
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
			role := &rbacv1.Role{TypeMeta: typeMeta("Role"), ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Rules: rules}
			if declared[d.Profile].Impersonate != nil {
				objs = append(objs, object{obj: role, note: fmt.Sprintf(
					"%s is not bound: profile %s acts as its callers, so bind it to those who use delivery tools.", name, d.Profile)})
				continue
			}
			objs = append(objs,
				object{obj: role},
				object{obj: roleBinding(name, ns, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}, []rbacv1.Subject{subject(d.Profile)})})
		}
		if _, ok := uses[d.Profile]; !ok {
			profiles = append(profiles, d.Profile)
		}
	}
	for _, profile := range profiles {
		if imp := declared[profile].Impersonate; imp != nil {
			o, err := impersonationRBAC(profile, imp, reach[profile], subject(profile))
			if err != nil {
				return nil, fmt.Errorf("cluster %s: %w", cluster, err)
			}
			objs = append(objs, o...)
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

// impersonationRBAC lets a profile's credential act as the principals that reach the
// profile, and no one else where they can be listed: users when no group or every
// principal reaches it, groups always (docs/design/0014).
func impersonationRBAC(profile string, imp *config.Impersonate, g policy.Grantees, subject rbacv1.Subject) ([]object, error) {
	name := rbacPrefix + profile + ":impersonate"
	var rules []rbacv1.PolicyRule
	var note string
	users := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}}
	switch {
	case g.Anyone || len(g.Groups) > 0:
		rules = append(rules, users)
		note = fmt.Sprintf("%s may act as any user name, the cluster's own system: users included, though Opsward never sends one: profile %s serves groups or every principal, whose members cannot be listed.", name, profile)
	case len(g.Users) > 0:
		for _, u := range g.Users {
			users.ResourceNames = append(users.ResourceNames, imp.UserPrefix()+u)
		}
		if err := noSystemNames(profile, "user", users.ResourceNames); err != nil {
			return nil, err
		}
		rules = append(rules, users)
	}
	if len(g.Groups) > 0 {
		groups := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"groups"}, Verbs: []string{"impersonate"}}
		for _, grp := range g.Groups {
			groups.ResourceNames = append(groups.ResourceNames, imp.GroupPrefix()+grp)
		}
		if err := noSystemNames(profile, "group", groups.ResourceNames); err != nil {
			return nil, err
		}
		rules = append(rules, groups)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return []object{
		{obj: clusterRole(name, rules), note: note},
		{obj: clusterRoleBinding(name, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}, []rbacv1.Subject{subject})},
	}, nil
}

// noSystemNames refuses to grant impersonation of the cluster's own identities, which
// Opsward never sends: system:masters is cluster-admin.
func noSystemNames(profile, kind string, names []string) error {
	for _, n := range names {
		if strings.HasPrefix(n, "system:") {
			return fmt.Errorf("profile %s would impersonate %s %q: system: names are the cluster's own; change the binding or the prefix", profile, kind, n)
		}
	}
	return nil
}
