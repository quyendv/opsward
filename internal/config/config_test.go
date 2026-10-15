package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
identity:
  principal: { subject: local-admin, groups: [admin] }
storage:
  path: /var/lib/opsward/opsward.db
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /etc/opsward/cluster-a-reader }
environments:
  - name: staging
    cluster: cluster-a
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Identity.Mode != "local" || c.Storage.Type != "sqlite" || c.Audit.Stream.Type != "stderr" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if got := c.Storage.AuditRetention.Std(); got != 90*24*time.Hour {
		t.Errorf("auditRetention = %v, want 90d", got)
	}
	if got := c.Audit.Timeout.Std(); got != 2*time.Second {
		t.Errorf("audit.timeout = %v, want 2s", got)
	}
	if c.Audit.Writer != "opsward" {
		t.Errorf("audit.writer = %q, want opsward: a host-name default starts a new chain in every container", c.Audit.Writer)
	}
	e := c.Environments[0]
	if len(e.Namespaces) != 1 || e.Namespaces[0] != "*" || e.Change.Strategy != "suggest" {
		t.Errorf("environment defaults not applied: %+v", e)
	}
	if len(c.Policy.Sources) != 1 || c.Policy.Sources[0] != "file" {
		t.Errorf("policy.sources = %v, want [file]", c.Policy.Sources)
	}
}

func TestParseKeepsExplicitZeroRetention(t *testing.T) {
	doc := strings.Replace(minimal, "  path: /var/lib/opsward/opsward.db", "  path: /var/lib/opsward/opsward.db\n  auditRetention: 0", 1)
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Storage.AuditRetention.Std(); got != 0 {
		t.Fatalf("auditRetention = %v, want 0 (keep forever)", got)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown field", minimal + "\nunknown: 1\n", "field unknown not found"},
		{"misspelled nested field", strings.Replace(minimal, "kubeconfig:", "kubeconfg:", 1), "field kubeconfg not found"},
		{"no subject", strings.Replace(minimal, "subject: local-admin, ", "", 1), "identity.principal.subject is required"},
		{"oidc not yet", strings.Replace(minimal, "identity:\n", "identity:\n  mode: oidc\n", 1), `identity.mode: "oidc" is not available`},
		{"no storage path", strings.Replace(minimal, "  path: /var/lib/opsward/opsward.db\n", "", 1), "storage.path is required"},
		{"bad retention", strings.Replace(minimal, "  path: /var/lib/opsward/opsward.db", "  path: x\n  auditRetention: soon", 1), `invalid duration "soon"`},
		{"unknown cluster", strings.Replace(minimal, "cluster: cluster-a", "cluster: cluster-z", 1), `unknown cluster "cluster-z"`},
		{"direct not yet", minimal + "    change: { strategy: direct }\n", `change strategy "direct" is not available`},
		{"file stream without path", minimal + "audit:\n  stream: { type: file }\n", "audit.stream.path is required"},
		{"tilde in kubeconfig", strings.Replace(minimal, "/etc/opsward/cluster-a-reader", "~/.kube/reader", 1), "~ is not expanded"},
		{"duplicate environment", minimal + "  - name: staging\n    cluster: cluster-a\n", `environment "staging": duplicate name`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "90d": 90 * 24 * time.Hour, "36h": 36 * time.Hour, "2s": 2 * time.Second} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestWarnings(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "reader.kubeconfig")
	if err := os.WriteFile(kc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	clean := strings.Replace(minimal, "/etc/opsward/cluster-a-reader", kc, 1)
	c, err := Parse([]byte(clean))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("clean config warnings = %v", w)
	}

	noisy := strings.Replace(minimal, "/var/lib/opsward/opsward.db", "opsward.db", 1)
	noisy = strings.Replace(noisy, "/etc/opsward/cluster-a-reader", "kube/reader", 1)
	noisy += "audit:\n  stream: { type: stdout }\n"
	c, err = Parse([]byte(noisy))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{
		`storage.path "opsward.db" is relative`,
		`kubeconfig "kube/reader" is relative`,
		`kubeconfig "kube/reader" does not exist`,
		"audit.stream.type stdout cannot be used by opsward serve",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings %q missing %q", got, want)
		}
	}
}

func TestKnowledgeSection(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Knowledge.BuiltinEnabled() || len(c.Knowledge.Paths) != 0 {
		t.Fatalf("knowledge defaults = %+v", c.Knowledge)
	}
	c, err = Parse([]byte(minimal + "knowledge:\n  builtin: false\n  paths: [/etc/opsward/knowledge]\n"))
	if err != nil || c.Knowledge.BuiltinEnabled() || c.Knowledge.Paths[0] != "/etc/opsward/knowledge" {
		t.Fatalf("knowledge = %+v, %v", c.Knowledge, err)
	}
	if _, err := Parse([]byte(minimal + "knowledge:\n  paths: [~/knowledge]\n")); err == nil || !strings.Contains(err.Error(), "~ is not expanded") {
		t.Fatalf("tilde path: err = %v", err)
	}
	c, _ = Parse([]byte(minimal + "knowledge:\n  paths: [kb]\n"))
	if !strings.Contains(strings.Join(c.Warnings(), "\n"), `knowledge path "kb" is relative`) {
		t.Fatalf("warnings = %v", c.Warnings())
	}
}

func TestRestartRequired(t *testing.T) {
	base, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		doc  string
		want string
	}{
		"policy only":   {minimal + "policy:\n  roles: {viewer: {risk: [read]}}\n", ""},
		"environments":  {strings.Replace(minimal, "    cluster: cluster-a\n", "    cluster: cluster-a\n    namespaces: [shop]\n", 1), ""},
		"knowledge":     {minimal + "knowledge:\n  builtin: false\n", ""},
		"storage path":  {strings.Replace(minimal, "/var/lib/opsward/opsward.db", "/var/lib/opsward/other.db", 1), "storage"},
		"identity":      {strings.Replace(minimal, "groups: [admin]", "groups: [admin, ops]", 1), "identity"},
		"audit timeout": {minimal + "audit:\n  timeout: 5s\n", "audit"},
	} {
		t.Run(name, func(t *testing.T) {
			next, err := Parse([]byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(base.RestartRequired(next), ","); got != tt.want {
				t.Fatalf("RestartRequired = %q, want %q", got, tt.want)
			}
		})
	}
}

// Equivalent identity, storage and audit sections must not force a restart (#10).
func TestRestartRequiredIgnoresEquivalentForms(t *testing.T) {
	doc := strings.Replace(minimal, "groups: [admin]", "groups: [admin, ops]", 1)
	base, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	for name, next := range map[string]string{
		"reordered groups":         strings.Replace(doc, "groups: [admin, ops]", "groups: [ops, admin]", 1),
		"duplicate group":          strings.Replace(doc, "groups: [admin, ops]", "groups: [ops, admin, ops]", 1),
		"retention as 90d spelled": strings.Replace(doc, "  path: /var/lib/opsward/opsward.db", "  path: /var/lib/opsward/opsward.db\n  auditRetention: 2160h", 1),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(next))
			if err != nil {
				t.Fatal(err)
			}
			if got := base.RestartRequired(c); len(got) != 0 {
				t.Fatalf("RestartRequired = %v, want none", got)
			}
		})
	}
	empty, _ := Parse([]byte(strings.Replace(minimal, "groups: [admin]", "groups: []", 1)))
	omitted, _ := Parse([]byte(strings.Replace(minimal, ", groups: [admin]", "", 1)))
	if got := empty.RestartRequired(omitted); len(got) != 0 {
		t.Fatalf("empty vs omitted groups: %v", got)
	}
}

const backends = `
  metrics:
    type: prometheus
    url: https://metrics.example.com
    clusterLabel: cluster
    auth: { bearerTokenFile: /etc/opsward/metrics-token }
  logs:
    type: loki
    url: https://logs.example.com
    tenant: team-a
`

func withBackends(doc, b string) string {
	return strings.Replace(doc, "providers:\n", "providers:"+b, 1)
}

func TestBackendsParseWithDefaults(t *testing.T) {
	c, err := Parse([]byte(withBackends(minimal, backends)))
	if err != nil {
		t.Fatal(err)
	}
	m, l := c.Providers.Metrics, c.Providers.Logs
	if m == nil || m.NamespaceLabel != "namespace" || m.ClusterLabel != "cluster" || m.Auth.BearerTokenFile == "" {
		t.Fatalf("metrics = %+v", m)
	}
	if l == nil || l.Type != "loki" || l.Tenant != "team-a" || l.NamespaceLabel != "namespace" || l.ClusterLabel != "" {
		t.Fatalf("logs = %+v", l)
	}
	if got := c.Environments[0].ClusterLabelValue; got != "cluster-a" {
		t.Fatalf("clusterLabelValue default = %q, want the cluster name", got)
	}
}

func TestBackendsReject(t *testing.T) {
	for name, tt := range map[string]struct{ b, want string }{
		"wrong metrics type": {"\n  metrics: { type: graphite, url: https://m.example.com }\n", `metrics.type: "graphite"`},
		"wrong logs type":    {"\n  logs: { type: elastic, url: https://l.example.com }\n", `logs.type: "elastic"`},
		"missing url":        {"\n  metrics: { type: prometheus }\n", "metrics.url is required"},
		"bad scheme":         {"\n  metrics: { type: prometheus, url: ftp://m.example.com }\n", "http or https"},
		"two auths":          {"\n  metrics: { type: prometheus, url: https://m.example.com, auth: { bearerTokenFile: /t, basic: { username: u, passwordFile: /p } } }\n", "one of bearerTokenFile and basic"},
		"basic no password":  {"\n  metrics: { type: prometheus, url: https://m.example.com, auth: { basic: { username: u } } }\n", "passwordFile is required"},
		"tilde token":        {"\n  logs: { type: loki, url: https://l.example.com, auth: { bearerTokenFile: ~/t } }\n", "~ is not expanded"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withBackends(minimal, tt.b)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBackendWarnings(t *testing.T) {
	c, err := Parse([]byte(withBackends(minimal, "\n  metrics: { type: prometheus, url: http://m.example.com, auth: { bearerTokenFile: /t } }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "credentials over plain http") {
		t.Fatalf("warnings = %s", w)
	}
}

func TestBackendsAreRestartOnly(t *testing.T) {
	base, _ := Parse([]byte(withBackends(minimal, backends)))
	moved, _ := Parse([]byte(withBackends(minimal, strings.Replace(backends, "https://metrics.example.com", "https://metrics2.example.com", 1))))
	if got := strings.Join(base.RestartRequired(moved), ","); got != "providers.metrics" {
		t.Fatalf("RestartRequired = %q", got)
	}
	relabel, _ := Parse([]byte(withBackends(strings.Replace(minimal, "    cluster: cluster-a\n", "    cluster: cluster-a\n    clusterLabelValue: prod-a\n", 1), backends)))
	if got := base.RestartRequired(relabel); len(got) != 0 {
		t.Fatalf("an environment's clusterLabelValue must be reloadable: %v", got)
	}
}

const twoClusters = `
identity:
  principal: { subject: me }
storage:
  path: /var/lib/opsward/o.db
providers:
  kubernetes:
    clusters:
      cluster-a: { profiles: { reader: { kubeconfig: /a } } }
      cluster-b: { profiles: { reader: { kubeconfig: /b } } }
%BACKEND%
environments:
  - { name: staging, cluster: cluster-a }
  - { name: production, cluster: cluster-b }
`

// A shared backend without clusterLabel cannot tell clusters apart: a caller in one
// environment would read the other cluster's namespace of the same name.
func TestBackendNeedsClusterLabelWithSeveralClusters(t *testing.T) {
	doc := strings.Replace(twoClusters, "%BACKEND%", "  metrics: { type: prometheus, url: https://m.example.com }", 1)
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "clusterLabel is required") {
		t.Fatalf("err = %v", err)
	}
	ok := strings.Replace(twoClusters, "%BACKEND%", "  metrics: { type: prometheus, url: https://m.example.com, clusterLabel: cluster }", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("with clusterLabel: %v", err)
	}
}

func TestClusterLabelValueWithoutClusterLabelWarns(t *testing.T) {
	doc := withBackends(strings.Replace(minimal, "    cluster: cluster-a\n", "    cluster: cluster-a\n    clusterLabelValue: prod-a\n", 1), "\n  metrics: { type: prometheus, url: https://m.example.com }\n")
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "clusterLabelValue") || !strings.Contains(w, "ignored") {
		t.Fatalf("warnings = %s", w)
	}
}

// Credentials are referenced by file, never inline (docs/design/0004).
func TestBackendURLMustNotCarryCredentialsOrQuery(t *testing.T) {
	for url, want := range map[string]string{
		"https://user:pass@m.example.com": "credentials",
		"https://user@m.example.com":      "credentials",
		"https://m.example.com/?org=1":    "query",
	} {
		_, err := Parse([]byte(withBackends(minimal, "\n  metrics: { type: prometheus, url: \""+url+"\" }\n")))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", url, err, want)
		}
	}
}

// Warnings are logged on every reload; they name a missing credential file by its field,
// never by its path.
func TestBackendWarningsDoNotNameCredentialPaths(t *testing.T) {
	c, err := Parse([]byte(withBackends(minimal, "\n  metrics: { type: prometheus, url: https://m.example.com, auth: { basic: { username: u, passwordFile: /run/secrets/metrics-password } } }\n")))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	if strings.Contains(w, "/run/secrets/metrics-password") || !strings.Contains(w, "auth.basic.passwordFile does not exist") {
		t.Fatalf("warnings = %s", w)
	}
}

// Issue #12: label names are checked when the configuration is loaded, not at query time.
func TestBackendLabelNames(t *testing.T) {
	for _, b := range []string{
		"\n  metrics: { type: prometheus, url: https://m.example.com, namespaceLabel: k8s.namespace }\n",
		"\n  logs: { type: loki, url: https://l.example.com, clusterLabel: \"1cluster\" }\n",
	} {
		if _, err := Parse([]byte(withBackends(minimal, b))); err == nil || !strings.Contains(err.Error(), "label name") {
			t.Errorf("%s: err = %v", b, err)
		}
	}
	if _, err := Parse([]byte(withBackends(minimal, "\n  metrics: { type: prometheus, url: https://m.example.com, namespaceLabel: k8s_namespace_name }\n"))); err != nil {
		t.Fatalf("valid label: %v", err)
	}
}
