package config

import (
	"strings"
	"testing"
)

func withImpersonation(block string) string {
	return strings.Replace(minimal, "reader: { kubeconfig: /etc/opsward/cluster-a-reader }",
		"reader: { kubeconfig: /etc/opsward/cluster-a-reader, impersonate: "+block+" }", 1)
}

func TestImpersonationDefaultsToOpswardPrefixes(t *testing.T) {
	c, err := Parse([]byte(withImpersonation("{}")))
	if err != nil {
		t.Fatal(err)
	}
	imp := c.Providers.Kubernetes.Clusters["cluster-a"].Profiles["reader"].Impersonate
	if imp == nil || imp.UserPrefix() != "opsward:" || imp.GroupPrefix() != "opsward:" {
		t.Fatalf("impersonate = %+v, want both prefixes opsward:", imp)
	}
	c, err = Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if imp := c.Providers.Kubernetes.Clusters["cluster-a"].Profiles["reader"].Impersonate; imp != nil {
		t.Fatalf("a profile without impersonate must not impersonate: %+v", imp)
	}
}

// An empty prefix is allowed, to reach the cluster's own names, and is worth a warning.
func TestImpersonationWithoutPrefixWarns(t *testing.T) {
	c, err := Parse([]byte(withImpersonation(`{ usernamePrefix: "", groupPrefix: "oidc:" }`)))
	if err != nil {
		t.Fatal(err)
	}
	imp := c.Providers.Kubernetes.Clusters["cluster-a"].Profiles["reader"].Impersonate
	if imp.UserPrefix() != "" || imp.GroupPrefix() != "oidc:" {
		t.Fatalf("prefixes %q %q", imp.UserPrefix(), imp.GroupPrefix())
	}
	got := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(got, `profile "reader": impersonate.usernamePrefix is empty`) {
		t.Fatalf("warnings %q do not mention the empty prefix", got)
	}
	if strings.Contains(got, "groupPrefix is empty") {
		t.Fatalf("warnings %q mention a prefix that is set", got)
	}
}

func TestImpersonationRejectsSystemPrefix(t *testing.T) {
	_, err := Parse([]byte(withImpersonation(`{ groupPrefix: "system:" }`)))
	if err == nil || !strings.Contains(err.Error(), `impersonate.groupPrefix "system:"`) {
		t.Fatalf("err = %v, want the system: prefix refused", err)
	}
}
