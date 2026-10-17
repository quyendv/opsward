package app

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
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
		"kind: RoleBinding\nmetadata:\n  name: opsward:reader\n  namespace: team-a-api",
		"kind: ClusterRoleBinding\nmetadata:\n  name: opsward:ops-b",
		"name: opsward:reader:delivery\n  namespace: argocd",
		"# opsward:reader is bound cluster-wide by the namespace patterns of environment production (*).\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: opsward:reader\n",
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

// A tool added without Needs would render no RBAC and fail at run time: every tool
// declares its operations, or is one that reaches no cluster.
func TestEveryToolDeclaresNeeds(t *testing.T) {
	doc := strings.Replace(rbacConfig, "identity:\n", "identity:\n  mode: remote\n  apiTokens: {}\n", 1)
	doc = strings.Replace(doc, "  principal: { subject: alice@example.com, groups: [team-a] }\n", "", 1)
	doc = strings.Replace(doc, "  delivery:", "  metrics: { type: prometheus, url: https://prometheus.example.com, clusterLabel: cluster }\n  logs: { type: loki, url: https://loki.example.com, clusterLabel: cluster }\n  delivery:", 1)
	cfg, err := config.Parse([]byte(doc + "server:\n  http: { address: 127.0.0.1:8080 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := checkTools(cfg)
	if err != nil {
		t.Fatal(err)
	}
	noCluster := []string{"list_environments", "search_knowledge", "get_knowledge", "query_metrics", "query_logs", "list_approvals", "decide_approval", "search_audit"}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if len(tl.Needs) == 0 && !slices.Contains(noCluster, tl.Name) {
			t.Errorf("%s declares no Needs", tl.Name)
		}
	}
	for _, want := range []string{"query_metrics", "list_approvals", "propose_change"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the configuration did not register %s: %v", want, names)
		}
	}
}

// Names derive from profile names; a profile named like another's suffix must not render
// an object with the same name, which kubectl apply would merge into one.
func TestRenderRBACNamesAreUnique(t *testing.T) {
	doc := strings.Replace(rbacConfig, "          operator: { kubeconfig: /etc/opsward/a-operator }",
		"          operator: { kubeconfig: /etc/opsward/a-operator }\n          reader-cluster: { kubeconfig: /etc/opsward/a-rc }\n          reader-delivery: { kubeconfig: /etc/opsward/a-rd }", 1)
	doc = strings.Replace(doc, "    - { name: view, subjects: { groups: [team-a] }, role: viewer }",
		"    - { name: view, subjects: { groups: [team-a] }, role: viewer }\n    - { name: rc, subjects: { groups: [team-a] }, role: restarter, profile: reader-cluster, scope: { environments: [staging] } }\n    - { name: rd, subjects: { groups: [team-a] }, role: restarter, profile: reader-delivery, scope: { environments: [staging] } }", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderRBAC(cfg, RBACOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range strings.Split(out, "# Cluster ")[1:] {
		uniqueObjects(t, section)
	}
}

func uniqueObjects(t *testing.T, section string) {
	t.Helper()
	seen := map[string]bool{}
	for _, d := range strings.Split(section, "\n---\n")[1:] {
		var kind, name, ns string
		for _, l := range strings.Split(d, "\n") {
			switch {
			case strings.HasPrefix(l, "kind: "):
				kind = l
			case strings.HasPrefix(l, "  name: ") && name == "":
				name = l
			case strings.HasPrefix(l, "  namespace: ") && ns == "":
				ns = l
			}
		}
		key := kind + "|" + ns + "|" + name
		if seen[key] {
			t.Fatalf("two objects are %s", key)
		}
		seen[key] = true
	}
}

// A --subject for a profile that renders nothing is a typo, not a no-op: the default
// subject would be bound instead.
func TestRenderRBACRefusesAnUnusedSubject(t *testing.T) {
	cfg, err := config.Parse([]byte(rbacConfig))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RenderRBAC(cfg, RBACOptions{Subjects: map[string]rbacv1.Subject{"oprator": {Kind: "User", Name: "x"}}})
	if err == nil || !strings.Contains(err.Error(), `"oprator"`) {
		t.Fatalf("err = %v", err)
	}
}

// The reader reads targets' labels before authorization only when a binding looks at
// them (apps or a labels condition); otherwise it needs no grant for that.
func TestRenderRBACReaderForLabelsOnlyWhenRead(t *testing.T) {
	doc := strings.Replace(rbacConfig, "    - { name: view, subjects: { groups: [team-a] }, role: viewer }\n", "", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderRBAC(cfg, RBACOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "name: opsward:reader\n") {
		t.Errorf("the reader is granted although no binding reads labels:\n%s", out)
	}
	doc = strings.Replace(doc, "role: restarter, scope: { environments: [staging] } }", "role: restarter, scope: { environments: [staging], apps: [checkout] } }", 1)
	if cfg, err = config.Parse([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if out, err = RenderRBAC(cfg, RBACOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: RoleBinding\nmetadata:\n  name: opsward:reader\n  namespace: team-a-api") {
		t.Errorf("the reader is not granted for an apps-scoped binding:\n%s", out)
	}
}

func TestRenderRBACApplicationNamespaces(t *testing.T) {
	doc := strings.Replace(rbacConfig, "  delivery: { type: argocd, cluster: cluster-a }", "  delivery: { type: argocd, cluster: cluster-a, applicationNamespaces: [team-a-apps] }", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderRBAC(cfg, RBACOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"argocd", "team-a-apps"} {
		for _, kind := range []string{"Role", "RoleBinding"} {
			if !strings.Contains(out, "kind: "+kind+"\nmetadata:\n  name: opsward:reader:delivery\n  namespace: "+ns+"\n") {
				t.Errorf("no %s in %s", kind, ns)
			}
		}
	}
}

// providers.delivery alone (no forge) still needs the delivery profile's get on
// Applications: get_delivery_status reads them.
func TestRenderRBACDeliveryWithoutForges(t *testing.T) {
	forgesAt, envsAt := strings.Index(rbacConfig, "  forges:\n"), strings.Index(rbacConfig, "environments:\n")
	if forgesAt < 0 || envsAt < forgesAt {
		t.Fatal("the fixture has no forges section before environments")
	}
	doc := rbacConfig[:forgesAt] + rbacConfig[envsAt:]
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderRBAC(cfg, RBACOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: Role\nmetadata:\n  name: opsward:reader:delivery\n  namespace: argocd") {
		t.Fatalf("no delivery Role:\n%s", out)
	}
}

// A chart installs what is rendered into one cluster: --cluster limits the output to it.
func TestRenderRBACOneCluster(t *testing.T) {
	cfg, err := config.Parse([]byte(rbacConfig))
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderRBAC(cfg, RBACOptions{Cluster: "cluster-b", Subjects: map[string]rbacv1.Subject{"ops-b": {Kind: "User", Name: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Cluster cluster-b") || strings.Contains(out, "# Cluster cluster-a") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := RenderRBAC(cfg, RBACOptions{Cluster: "cluster-z"}); err == nil || !strings.Contains(err.Error(), "cluster-z") {
		t.Fatalf("unknown cluster: err = %v", err)
	}
}
