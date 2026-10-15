package config

import (
	"net"
	"net/url"
	"strings"
)

// OIDC configures access tokens from an OpenID Connect issuer. docs/design/0005.
type OIDC struct {
	Issuer string `yaml:"issuer"`
	// Audience must appear in a token's aud claim; default the MCP resource URL,
	// publicURL + "/mcp", which clients request as the resource indicator.
	Audience     string `yaml:"audience"`
	SubjectClaim string `yaml:"subjectClaim"` // default email
	GroupsClaim  string `yaml:"groupsClaim"`  // default groups
	// GroupsPrefix is put before every claim group, "oidc:" unless set, even to "":
	// without one, a group created at the issuer would be the internal group of the
	// same name.
	GroupsPrefix *string  `yaml:"groupsPrefix"`
	Scopes       []string `yaml:"scopes"` // advertised in the resource metadata
}

// DefaultGroupsPrefix is put before claim groups when groupsPrefix is not set.
const DefaultGroupsPrefix = "oidc:"

// GroupsPrefixOrDefault returns the configured prefix or DefaultGroupsPrefix.
func (o *OIDC) GroupsPrefixOrDefault() string {
	if o.GroupsPrefix == nil {
		return DefaultGroupsPrefix
	}
	return *o.GroupsPrefix
}

// ResourceURL is the MCP endpoint as clients reach it, or "" without a publicURL.
func (c *Config) ResourceURL() string {
	if c.Server.HTTP == nil || c.Server.HTTP.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(c.Server.HTTP.PublicURL, "/") + "/mcp"
}

func (c *Config) oidcDefaults() {
	o := c.Identity.OIDC
	if o == nil {
		return
	}
	if o.Audience == "" {
		o.Audience = c.ResourceURL()
	}
	if o.SubjectClaim == "" {
		o.SubjectClaim = "email"
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
}

func (c *Config) validateOIDC(fail func(string, ...any)) {
	o := c.Identity.OIDC
	if o == nil {
		return
	}
	if c.Identity.Mode != ModeRemote {
		fail("identity.oidc is used only in remote mode")
		return
	}
	if o.Issuer == "" {
		fail("identity.oidc.issuer is required")
	} else if !validIssuer(o.Issuer) {
		fail("identity.oidc.issuer %q must be an https URL without query or fragment (http only on loopback)", o.Issuer)
	}
	if c.ResourceURL() == "" {
		fail("server.http.publicURL is required with identity.oidc: clients find the issuer through the resource metadata it names")
	}
	if o.Audience == "" {
		fail("identity.oidc.audience is required")
	}
	if strings.TrimSpace(o.SubjectClaim) == "" || strings.TrimSpace(o.GroupsClaim) == "" {
		fail("identity.oidc.subjectClaim and groupsClaim must not be blank")
	}
}

func validIssuer(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}
