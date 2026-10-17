package config

import (
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Console configures the web console (docs/design/0015); restart-only.
type Console struct {
	SessionTTL *Duration    `yaml:"sessionTTL"` // default 8h, at most 24h
	OIDC       *ConsoleOIDC `yaml:"oidc"`       // sign in with identity.oidc's issuer
	APITokens  bool         `yaml:"apiTokens"`  // sign in with an API token
}

// ConsoleOIDC is the console's client at the issuer.
type ConsoleOIDC struct {
	ClientID     string     `yaml:"clientID"`
	ClientSecret *SecretRef `yaml:"clientSecret"` // nil: a public client, with PKCE only
	Scopes       []string   `yaml:"scopes"`       // requested besides openid
}

// LimitConsoleSessionTTL bounds console.sessionTTL: the issuer's groups are fixed for a
// session's life.
const LimitConsoleSessionTTL = 24 * time.Hour

// RedirectURL is where the issuer sends a person back to after signing in.
func (k *Console) RedirectURL(publicURL string) string {
	return strings.TrimSuffix(publicURL, "/") + "/console/callback"
}

func (c *Config) consoleDefaults() {
	if c.Console != nil && c.Console.SessionTTL == nil {
		d := Duration(8 * time.Hour)
		c.Console.SessionTTL = &d
	}
}

func (c *Config) validateConsole(fail func(string, ...any)) {
	k := c.Console
	if k == nil {
		return
	}
	if c.Identity.Mode != ModeRemote {
		fail("console is served only in remote mode")
		return
	}
	if k.OIDC == nil && !k.APITokens {
		fail("console: set oidc, apiTokens, or both: there is no way to sign in")
	}
	if d := k.SessionTTL.Std(); d <= 0 || d > LimitConsoleSessionTTL {
		fail("console.sessionTTL must be positive and at most 24h")
	}
	o := k.OIDC
	if o == nil {
		return
	}
	if c.Identity.OIDC == nil {
		fail("console.oidc signs in with identity.oidc's issuer, which is not configured")
	}
	if c.Server.HTTP == nil || c.Server.HTTP.PublicURL == "" {
		fail("console.oidc needs server.http.publicURL: the issuer sends people back to <publicURL>/console/callback")
	}
	if o.ClientID == "" {
		fail("console.oidc.clientID is required")
	}
	if o.ClientSecret != nil && o.ClientSecret.File == "" && o.ClientSecret.Env == "" {
		fail("console.oidc.clientSecret: { file } or { env } is required")
	}
	if slices.Contains(o.Scopes, "openid") {
		fail("console.oidc.scopes: openid is always requested; list only the others")
	}
}

func (c *Config) consoleWarnings() []string {
	h := c.Server.HTTP
	if c.Console == nil || h == nil {
		return nil
	}
	loopback := func(host string) bool { return host == "localhost" || net.ParseIP(host).IsLoopback() }
	if h.PublicURL == "" {
		// Where people reach it is unknown: judge by where it listens.
		host, _, err := net.SplitHostPort(h.Address)
		if err != nil || h.TLS != nil || loopback(host) {
			return nil
		}
		return []string{"console: browsers keep the session cookie only over HTTPS or on localhost, and server.http serves plain HTTP; terminate TLS in front of it"}
	}
	u, err := url.Parse(h.PublicURL)
	if err != nil || u.Scheme != "http" || loopback(u.Hostname()) {
		return nil
	}
	return []string{"console: browsers keep the session cookie only over HTTPS or on localhost, and server.http.publicURL is plain HTTP"}
}
