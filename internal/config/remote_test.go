package config

import (
	"strings"
	"testing"
	"time"
)

const remote = `
identity:
  mode: remote
  groups:
    platform: [alice@example.com, bob@example.com]
server:
  http:
    address: 127.0.0.1:8080
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

func TestRemoteParsesWithDefaults(t *testing.T) {
	c, err := Parse([]byte(remote))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Identity.Mode != "remote" || c.Server.HTTP.Address != "127.0.0.1:8080" {
		t.Fatalf("identity/server: %+v %+v", c.Identity, c.Server)
	}
	if got := c.Identity.APITokens.MaxTTL.Std(); got != 365*24*time.Hour {
		t.Errorf("apiTokens.maxTTL = %v, want 365d", got)
	}
	if got := c.Identity.Groups["platform"]; len(got) != 2 || got[0] != "alice@example.com" {
		t.Errorf("groups = %v", c.Identity.Groups)
	}
}

func TestRemoteRejects(t *testing.T) {
	for name, tt := range map[string]struct{ doc, want string }{
		"fixed principal": {strings.Replace(remote, "  mode: remote\n", "  mode: remote\n  principal: { subject: admin }\n", 1), "identity.principal is used only in local mode"},
		"no address":      {strings.Replace(remote, "    address: 127.0.0.1:8080\n", "    publicURL: https://opsward.example.com\n", 1), "server.http.address is required in remote mode"},
		"bad address":     {strings.Replace(remote, "127.0.0.1:8080", "8080", 1), `server.http.address "8080"`},
		"half tls":        {strings.Replace(remote, "    address: 127.0.0.1:8080\n", "    address: 127.0.0.1:8080\n    tls: { certFile: /etc/opsward/tls.crt }\n", 1), "server.http.tls: certFile and keyFile are both required"},
		"public url":      {strings.Replace(remote, "    address: 127.0.0.1:8080\n", "    address: 127.0.0.1:8080\n    publicURL: opsward.example.com\n", 1), "server.http.publicURL"},
		"public url path": {strings.Replace(remote, "    address: 127.0.0.1:8080\n", "    address: 127.0.0.1:8080\n    publicURL: https://opsward.example.com/x?y=1\n", 1), "server.http.publicURL"},
		"zero max ttl":    {strings.Replace(remote, "  mode: remote\n", "  mode: remote\n  apiTokens: { maxTTL: 0 }\n", 1), "identity.apiTokens.maxTTL must be positive"},
		"empty member":    {strings.Replace(remote, "[alice@example.com, bob@example.com]", `[alice@example.com, ""]`, 1), `identity.groups.platform: empty subject`},
		"empty group":     {strings.Replace(remote, "    platform: [", "    \"\": [", 1), "identity.groups: empty group name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A listener with a fixed principal would hand that principal to anyone who can connect.
func TestLocalRejectsServerHTTP(t *testing.T) {
	doc := minimal + "server:\n  http: { address: 127.0.0.1:8080 }\n"
	_, err := Parse([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "server.http is used only in remote mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestInternalGroupsReloadWithoutRestart(t *testing.T) {
	base, err := Parse([]byte(remote))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct{ doc, want string }{
		"member added":  {strings.Replace(remote, "bob@example.com]", "bob@example.com, carol@example.com]", 1), ""},
		"group removed": {strings.Replace(remote, "  groups:\n    platform: [alice@example.com, bob@example.com]\n", "", 1), ""},
		"max ttl":       {strings.Replace(remote, "  mode: remote\n", "  mode: remote\n  apiTokens: { maxTTL: 30d }\n", 1), "identity"},
		"address":       {strings.Replace(remote, "127.0.0.1:8080", "127.0.0.1:9090", 1), "server"},
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

func TestRemoteWarnings(t *testing.T) {
	for name, tt := range map[string]struct {
		doc  string
		want bool
	}{
		"loopback":         {remote, false},
		"all interfaces":   {strings.Replace(remote, "127.0.0.1:8080", "0.0.0.0:8080", 1), true},
		"empty host":       {strings.Replace(remote, "127.0.0.1:8080", ":8080", 1), true},
		"all ifaces + tls": {strings.Replace(remote, "    address: 127.0.0.1:8080\n", "    address: 0.0.0.0:8443\n    tls: { certFile: /etc/opsward/tls.crt, keyFile: /etc/opsward/tls.key }\n", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(strings.Join(c.Warnings(), "\n"), "plain HTTP")
			if got != tt.want {
				t.Fatalf("plain HTTP warning = %v, want %v; warnings: %v", got, tt.want, c.Warnings())
			}
		})
	}
}
