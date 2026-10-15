package config

import (
	"fmt"
	"net"
	"net/url"
	"time"
)

// Identity modes. docs/design/0005.
const (
	ModeLocal  = "local"
	ModeRemote = "remote"
)

// DefaultAPITokenMaxTTL is the longest lifetime an API token can be created with.
const DefaultAPITokenMaxTTL = 365 * 24 * time.Hour

// APITokens configures API tokens, which are always accepted in remote mode.
type APITokens struct {
	MaxTTL *Duration `yaml:"maxTTL"`
}

// Server configures listeners; remote mode only.
type Server struct {
	HTTP *HTTP `yaml:"http"`
}

// HTTP configures the MCP over HTTP listener.
type HTTP struct {
	Address string `yaml:"address"`
	// PublicURL is how clients reach Opsward, used in OAuth resource metadata.
	PublicURL string `yaml:"publicURL"`
	TLS       *TLS   `yaml:"tls"`
}

// TLS references a certificate and key file, re-read when they change.
type TLS struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

func (c *Config) validateIdentity(fail func(string, ...any)) {
	id := c.Identity
	switch id.Mode {
	case ModeLocal:
		if id.Principal.Subject == "" {
			fail("identity.principal.subject is required in local mode")
		}
		if c.Server.HTTP != nil {
			fail("server.http is used only in remote mode: a listener with a fixed principal would give it to anyone who connects")
		}
	case ModeRemote:
		if id.Principal.Subject != "" || len(id.Principal.Groups) > 0 {
			fail("identity.principal is used only in local mode; in remote mode each request's token names its principal")
		}
		c.validateHTTP(fail)
	default:
		fail("identity.mode: unknown mode %q; use %s or %s", id.Mode, ModeLocal, ModeRemote)
	}
	if *id.APITokens.MaxTTL <= 0 {
		fail("identity.apiTokens.maxTTL must be positive")
	}
	for g, members := range id.Groups {
		if g == "" {
			fail("identity.groups: empty group name")
			continue
		}
		for _, m := range members {
			if m == "" {
				fail("identity.groups.%s: empty subject", g)
			}
		}
	}
}

func (c *Config) validateHTTP(fail func(string, ...any)) {
	h := c.Server.HTTP
	if h == nil || h.Address == "" {
		fail("server.http.address is required in remote mode")
		return
	}
	if _, _, err := net.SplitHostPort(h.Address); err != nil {
		fail("server.http.address %q: want host:port, such as 0.0.0.0:8080", h.Address)
	}
	if h.PublicURL != "" {
		u, err := url.Parse(h.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			fail("server.http.publicURL %q must be an http or https URL without credentials, query or fragment", h.PublicURL)
		}
	}
	if h.TLS != nil && (h.TLS.CertFile == "" || h.TLS.KeyFile == "") {
		fail("server.http.tls: certFile and keyFile are both required")
	}
}

func (c *Config) remoteWarnings() []string {
	h := c.Server.HTTP
	if c.Identity.Mode != ModeRemote || h == nil || h.TLS != nil {
		return nil
	}
	host, _, err := net.SplitHostPort(h.Address)
	if err != nil {
		return nil // already a validation error
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return []string{fmt.Sprintf("server.http.address %s serves plain HTTP beyond this host: bearer tokens cross the network unencrypted unless TLS is terminated in front", h.Address)}
}
