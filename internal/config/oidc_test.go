package config

import (
	"strings"
	"testing"
)

func withOIDC(oidc string) string {
	doc := strings.Replace(remote, "  mode: remote\n", "  mode: remote\n  oidc:\n"+oidc, 1)
	return strings.Replace(doc, "    address: 127.0.0.1:8080\n", "    address: 127.0.0.1:8080\n    publicURL: https://opsward.example.com\n", 1)
}

func TestOIDCDefaults(t *testing.T) {
	c, err := Parse([]byte(withOIDC("    issuer: https://id.example.com\n")))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Identity.OIDC
	if o.Audience != "https://opsward.example.com/mcp" || o.SubjectClaim != "email" || o.GroupsClaim != "groups" || o.GroupsPrefixOrDefault() != "oidc:" {
		t.Fatalf("defaults = %+v", o)
	}
}

func TestOIDCRejects(t *testing.T) {
	for name, tt := range map[string]struct{ doc, want string }{
		"no issuer":       {withOIDC("    audience: opsward\n"), "identity.oidc.issuer is required"},
		"http issuer":     {withOIDC("    issuer: http://id.example.com\n"), "identity.oidc.issuer"},
		"issuer w/ query": {withOIDC("    issuer: https://id.example.com/?x=1\n"), "identity.oidc.issuer"},
		"no public url": {
			strings.Replace(withOIDC("    issuer: https://id.example.com\n"), "    publicURL: https://opsward.example.com\n", "", 1),
			"server.http.publicURL is required with identity.oidc",
		},
		"local mode": {
			strings.Replace(minimal, "identity:\n", "identity:\n  oidc: { issuer: https://id.example.com }\n", 1),
			"identity.oidc is used only in remote mode",
		},
		"empty subject claim": {withOIDC("    issuer: https://id.example.com\n    subjectClaim: \" \"\n"), "identity.oidc.subjectClaim"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// A local issuer over plain HTTP is accepted for development, on loopback only.
func TestOIDCLoopbackHTTPIssuer(t *testing.T) {
	if _, err := Parse([]byte(withOIDC("    issuer: http://127.0.0.1:5556/dex\n"))); err != nil {
		t.Fatal(err)
	}
}

// Claim groups must not merge with internal groups by name unless the operator says so:
// at many issuers users create their own groups.
func TestOIDCGroupsPrefixDefaultsToOIDC(t *testing.T) {
	c, err := Parse([]byte(withOIDC("    issuer: https://id.example.com\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Identity.OIDC.GroupsPrefixOrDefault(); got != "oidc:" {
		t.Fatalf("default prefix = %q", got)
	}
	c, err = Parse([]byte(withOIDC("    issuer: https://id.example.com\n    groupsPrefix: \"\"\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Identity.OIDC.GroupsPrefixOrDefault(); got != "" {
		t.Fatalf("explicit empty prefix = %q", got)
	}
}
