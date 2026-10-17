package app

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/quyendv/opsward/internal/config"
)

// cluster-a's profiles act as their callers; cluster-b's do not. The delivery profile,
// cluster-a's reader, reads Applications for every caller of a delivery tool.
const impersonationConfig = `
identity:
  principal: { subject: alice@example.com, groups: [team-a] }
storage:
  path: /var/lib/opsward/opsward.db
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /etc/opsward/a-reader, impersonate: {} }
          operator: { kubeconfig: /etc/opsward/a-operator, impersonate: { usernamePrefix: "oidc:", groupPrefix: "oidc:" } }
      cluster-b:
        profiles:
          reader: { kubeconfig: /etc/opsward/b-reader }
  delivery: { type: argocd, cluster: cluster-a }
environments:
  - name: staging
    cluster: cluster-a
    namespaces: [team-a-web, team-a-api]
  - name: production
    cluster: cluster-b
policy:
  roles:
    viewer: { risk: [read] }
    restarter: { tools: [restart_workload] }
  bindings:
    - { name: view, subjects: { groups: [team-a] }, role: viewer }
    - { name: restart-staging, subjects: { users: [Bob@Example.com] }, role: restarter, scope: { environments: [staging] } }
    - { name: ops-view, subjects: { groups: [ops] }, role: viewer, scope: { environments: [production] } }
`

func TestDeriveGivesImpersonatingProfilesTheirGroups(t *testing.T) {
	cfg, err := config.Parse([]byte(impersonationConfig))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := checkTools(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := derive(cfg, tools, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reader, operator := d.profiles["cluster-a"]["reader"], d.profiles["cluster-a"]["operator"]
	// The reader serves staging's viewers and, as the delivery profile, every caller of a
	// delivery tool, production's included.
	if i := reader.Impersonate; i == nil || i.UserPrefix != "opsward:" || !slices.Equal(i.Groups, []string{"ops", "team-a"}) {
		t.Fatalf("cluster-a reader impersonates %+v", i)
	}
	if i := operator.Impersonate; i == nil || i.UserPrefix != "oidc:" || i.GroupPrefix != "oidc:" || len(i.Groups) != 0 {
		t.Fatalf("cluster-a operator impersonates %+v", i)
	}
	if i := d.profiles["cluster-b"]["reader"].Impersonate; i != nil {
		t.Fatalf("cluster-b reader impersonates %+v, want its own credential", i)
	}
}

func TestRenderRBACForImpersonatingProfiles(t *testing.T) {
	cfg, err := config.Parse([]byte(impersonationConfig))
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderRBAC(cfg, RBACOptions{Cluster: "cluster-a"})
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(got, "---\n")
	find := func(kind, name string) string {
		t.Helper()
		for _, d := range docs {
			if strings.Contains(d, "kind: "+kind+"\n") && strings.Contains(d, "  name: "+name+"\n") {
				return d
			}
		}
		return ""
	}
	// The operator serves only Bob, a user, so it may impersonate only him.
	op := find("ClusterRole", "opsward:operator:impersonate")
	for _, want := range []string{"- impersonate", "- users", "- oidc:bob@example.com"} {
		if !strings.Contains(op, want) {
			t.Errorf("operator impersonation role lacks %q:\n%s", want, op)
		}
	}
	if strings.Contains(op, "- groups") {
		t.Errorf("operator impersonation role names groups, but nobody reaches it through one:\n%s", op)
	}
	// The reader serves groups, whose members cannot be listed: any user, listed groups.
	rd := find("ClusterRole", "opsward:reader:impersonate")
	for _, want := range []string{"- users", "- groups", "- opsward:ops", "- opsward:team-a"} {
		if !strings.Contains(rd, want) {
			t.Errorf("reader impersonation role lacks %q:\n%s", want, rd)
		}
	}
	if !strings.Contains(got, "opsward:reader:impersonate may act as any user name") {
		t.Errorf("no note that the reader may act as any user:\n%s", got)
	}
	if b := find("ClusterRoleBinding", "opsward:reader:impersonate"); !strings.Contains(b, "name: opsward-reader") {
		t.Errorf("the impersonation role is not bound to the reader's ServiceAccount:\n%s", b)
	}
	// What the tools need is rendered for the people, unbound.
	if find("ClusterRole", "opsward:reader") == "" || find("Role", "opsward:reader:delivery") == "" {
		t.Errorf("the rules the tools need are missing:\n%s", got)
	}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, name := range []string{"opsward:reader", "opsward:operator", "opsward:reader:delivery"} {
			if find(kind, name) != "" {
				t.Errorf("%s %s binds an impersonating profile's rules to its credential", kind, name)
			}
		}
	}
	if !strings.Contains(got, "opsward:reader is not bound") || !strings.Contains(got, "team-a-api, team-a-web") {
		t.Errorf("no note on binding the reader's rules to people:\n%s", got)
	}
}

// Opsward never sends a system: name, so the credential must not be granted one either:
// impersonating system:masters is cluster-admin.
func TestRenderRBACRefusesSystemNames(t *testing.T) {
	for name, edit := range map[string][2]string{
		"group across the prefix": {"impersonate: {} }", `impersonate: { groupPrefix: "sys" } }`},
		"unprefixed group":        {"impersonate: {} }", `impersonate: { groupPrefix: "" } }`},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(impersonationConfig, edit[0], edit[1], 1)
			group := "system:masters"
			if edit[1] == `impersonate: { groupPrefix: "sys" } }` {
				group = "tem:masters"
			}
			doc = strings.Replace(doc, "groups: [team-a] }, role: viewer", "groups: [team-a, "+group+"] }, role: viewer", 1)
			cfg, err := config.Parse([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			_, err = RenderRBAC(cfg, RBACOptions{Cluster: "cluster-a"})
			if err == nil || !strings.Contains(err.Error(), `"system:masters"`) {
				t.Fatalf("err = %v, want system:masters refused", err)
			}
		})
	}
	doc := strings.Replace(impersonationConfig, `usernamePrefix: "oidc:", groupPrefix: "oidc:"`, `usernamePrefix: ""`, 1)
	doc = strings.Replace(doc, "users: [Bob@Example.com]", "users: [system:kube-controller-manager]", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderRBAC(cfg, RBACOptions{Cluster: "cluster-a"}); err == nil || !strings.Contains(err.Error(), "system:kube-controller-manager") {
		t.Fatalf("err = %v, want the system: user refused", err)
	}
}

// A --subject for a profile no rendered object binds is a mistake, impersonating or not.
func TestRenderRBACSubjectForAnImpersonatingProfileThatBindsNothing(t *testing.T) {
	doc := strings.Replace(impersonationConfig, "  delivery: { type: argocd, cluster: cluster-a }",
		"  delivery: { type: argocd, cluster: cluster-a, profile: operator }", 1)
	// Nobody reaches the operator, so it renders no impersonation and binds nothing.
	doc = strings.Replace(doc, "    - { name: restart-staging, subjects: { users: [Bob@Example.com] }, role: restarter, scope: { environments: [staging] } }\n", "", 1)
	doc = strings.Replace(doc, "    - { name: view, subjects: { groups: [team-a] }, role: viewer }\n", "", 1)
	doc = strings.Replace(doc, "    - { name: ops-view, subjects: { groups: [ops] }, role: viewer, scope: { environments: [production] } }\n", "", 1)
	doc = strings.Replace(doc, "  bindings:\n", "  bindings: []\n", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	p, subject, err := ParseRBACSubject("operator=User:someone")
	if err != nil {
		t.Fatal(err)
	}
	_, err = RenderRBAC(cfg, RBACOptions{Cluster: "cluster-a", Subjects: map[string]rbacv1.Subject{p: subject}})
	if err == nil || !strings.Contains(err.Error(), `--subject names profile "operator"`) {
		t.Fatalf("err = %v, want the unused --subject refused", err)
	}
}
