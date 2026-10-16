package app

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/quyendv/opsward/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Two clusters: cluster-a serves an environment of literal namespaces (RoleBindings) and
// holds the Argo CD Applications; cluster-b serves every namespace (ClusterRoleBindings).
const rbacConfig = `
identity:
  principal: { subject: alice@example.com, groups: [team-a] }
storage:
  path: /var/lib/opsward/opsward.db
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /etc/opsward/a-reader }
          operator: { kubeconfig: /etc/opsward/a-operator }
      cluster-b:
        profiles:
          reader: { kubeconfig: /etc/opsward/b-reader }
          ops-b: { kubeconfig: /etc/opsward/b-ops }
  delivery: { type: argocd, cluster: cluster-a }
  forges:
    - { name: git, type: gitea, url: https://git.example.com, hosts: [git.example.com], token: { file: /etc/opsward/git-token }, repositories: [team-a/gitops] }
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
    scaler: { tools: [scale_workload] }
  bindings:
    - { name: view, subjects: { groups: [team-a] }, role: viewer }
    - { name: restart-staging, subjects: { groups: [team-a] }, role: restarter, scope: { environments: [staging] } }
    - name: scale-production
      subjects: { groups: [team-a] }
      role: scaler
      profile: ops-b
      scope: { environments: [production] }
      effect: approve
      approvers: { groups: [platform] }
`

func TestRenderRBAC(t *testing.T) {
	cfg, err := config.Parse([]byte(rbacConfig))
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderRBAC(cfg, RBACOptions{
		Subjects: map[string]rbacv1.Subject{"ops-b": {Kind: "User", Name: "opsward-ops-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "rbac.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // G304: the test's own golden file
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s (go test -run TestRenderRBAC -update):\n%s", golden, got)
	}
	for _, must := range []string{
		"kind: RoleBinding\nmetadata:\n  name: opsward-reader\n  namespace: team-a-api",
		"kind: ClusterRoleBinding\nmetadata:\n  name: opsward-ops-b",
		"name: opsward-reader-delivery\n  namespace: argocd",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("output misses %q", must)
		}
	}
}

func TestRenderRBACSubjectFlag(t *testing.T) {
	for in, want := range map[string]rbacv1.Subject{
		"reader=ServiceAccount:ops/opsward": {Kind: "ServiceAccount", Namespace: "ops", Name: "opsward"},
		"reader=User:opsward-reader":        {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "opsward-reader"},
		"reader=Group:opsward":              {Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "opsward"},
	} {
		profile, s, err := ParseRBACSubject(in)
		if err != nil || profile != "reader" || s != want {
			t.Errorf("ParseRBACSubject(%q) = %q, %+v, %v", in, profile, s, err)
		}
	}
	for _, bad := range []string{"reader", "reader=Robot:x", "reader=ServiceAccount:x", "=User:x", "reader=User:"} {
		if _, _, err := ParseRBACSubject(bad); err == nil {
			t.Errorf("ParseRBACSubject(%q) accepted", bad)
		}
	}
}
