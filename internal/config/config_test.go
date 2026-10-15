package config

import (
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
	if c.Audit.Writer == "" {
		t.Error("audit.writer should default to the host name")
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
