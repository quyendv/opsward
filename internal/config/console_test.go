package config

import (
	"strings"
	"testing"
	"time"
)

const oidcIdentity = `
  oidc:
    issuer: https://issuer.example.com
`

// consoleRemote is remote with OIDC and a public URL, as a console signing in with
// OIDC needs.
var consoleRemote = strings.Replace(strings.Replace(remote, "  mode: remote\n", "  mode: remote\n"+strings.TrimPrefix(oidcIdentity, "\n"), 1),
	"    address: 127.0.0.1:8080\n", "    address: 127.0.0.1:8080\n    publicURL: https://opsward.example.com\n", 1)

const consoleSection = `
console:
  oidc:
    clientID: opsward-console
    clientSecret: { file: /etc/opsward/console/client-secret }
    scopes: [groups]
`

func TestConsoleParsesWithDefaults(t *testing.T) {
	c, err := Parse([]byte(consoleRemote + consoleSection))
	if err != nil {
		t.Fatal(err)
	}
	k := c.Console
	if k == nil || k.SessionTTL.Std() != 8*time.Hour || k.OIDC.ClientID != "opsward-console" || k.OIDC.ClientSecret.File == "" || k.APITokens {
		t.Fatalf("console = %+v", k)
	}
	if got := k.RedirectURL(c.Server.HTTP.PublicURL); got != "https://opsward.example.com/console/callback" {
		t.Fatalf("redirect URL = %q", got)
	}
	if c, err := Parse([]byte(remote + "console:\n  apiTokens: true\n")); err != nil || c.Console.OIDC != nil {
		t.Fatalf("a console signing in with API tokens only: %v", err)
	}
}

func TestConsoleRejects(t *testing.T) {
	local := strings.Replace(remote, "  mode: remote\n", "  mode: local\n  principal: { subject: alice }\n", 1)
	local = strings.Replace(local, "server:\n  http:\n    address: 127.0.0.1:8080\n", "", 1)
	local = strings.Replace(local, "  groups:\n    platform: [alice@example.com, bob@example.com]\n", "", 1)
	tests := []struct{ name, doc, want string }{
		{"local mode", local + "console:\n  apiTokens: true\n", "console is served only in remote mode"},
		{"no sign-in", remote + "console: {}\n", "console: set oidc, apiTokens, or both"},
		{"no identity.oidc", remote + consoleSection, "console.oidc signs in with identity.oidc's issuer"},
		{"no public URL", strings.Replace(consoleRemote, "    publicURL: https://opsward.example.com\n", "", 1) + consoleSection, "console.oidc needs server.http.publicURL"},
		{"no client", consoleRemote + strings.Replace(consoleSection, "    clientID: opsward-console\n", "", 1), "console.oidc.clientID is required"},
		{"inline secret", consoleRemote + strings.Replace(consoleSection, "{ file: /etc/opsward/console/client-secret }", "s3cr3t", 1), "an inline value is not accepted"},
		{"long session", consoleRemote + consoleSection + "  sessionTTL: 25h\n", "console.sessionTTL must be positive and at most 24h"},
		{"openid scope", consoleRemote + strings.Replace(consoleSection, "[groups]", "[openid, groups]", 1), "console.oidc.scopes: openid is always requested"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// Browsers keep a __Host- cookie only over HTTPS or on the loopback interface.
func TestConsoleWarnsAboutPlainHTTP(t *testing.T) {
	doc := strings.Replace(consoleRemote, "https://opsward.example.com", "http://opsward.example.com", 1) + consoleSection
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings() {
		found = found || strings.Contains(w, "console: browsers keep the session cookie only over HTTPS")
	}
	if !found {
		t.Fatalf("warnings = %q", c.Warnings())
	}
}

func TestConsoleIsRestartOnly(t *testing.T) {
	a, _ := Parse([]byte(consoleRemote + consoleSection))
	b, _ := Parse([]byte(consoleRemote + consoleSection + "  sessionTTL: 1h\n"))
	if got := a.RestartRequired(b); len(got) != 1 || got[0] != "console" {
		t.Fatalf("RestartRequired = %v", got)
	}
}

// Without a public URL, plain HTTP beyond this host is warned about for the console too.
func TestConsoleWarnsAboutPlainHTTPWithoutAPublicURL(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(remote, "127.0.0.1:8080", "0.0.0.0:8080", 1) + "console:\n  apiTokens: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings() {
		found = found || strings.Contains(w, "console: browsers keep the session cookie only over HTTPS")
	}
	if !found {
		t.Fatalf("warnings = %q", c.Warnings())
	}
	if c, _ := Parse([]byte(remote + "console:\n  apiTokens: true\n")); len(c.consoleWarnings()) != 0 {
		t.Fatalf("on loopback: %q", c.consoleWarnings())
	}
}
