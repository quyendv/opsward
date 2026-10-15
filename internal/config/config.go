// Package config loads and validates the Opsward configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/quyendv/opsward/internal/policy"
)

// Defaults applied when a field is omitted.
const (
	DefaultAuditRetention = 90 * 24 * time.Hour
	DefaultAuditTimeout   = 2 * time.Second
	// DefaultAuditWriter is a constant rather than the host name: a host name changes
	// with every container, and each change would start a new chain.
	DefaultAuditWriter = "opsward"
)

// Config is the whole configuration file.
type Config struct {
	Identity     Identity      `yaml:"identity"`
	Storage      Storage       `yaml:"storage"`
	Audit        Audit         `yaml:"audit"`
	Providers    Providers     `yaml:"providers"`
	Environments []Environment `yaml:"environments"`
	Policy       policy.Config `yaml:"policy"`
}

// Identity configures how callers are identified.
type Identity struct {
	Mode      string    `yaml:"mode"`
	Principal Principal `yaml:"principal"`
}

// Principal is the fixed caller in local mode.
type Principal struct {
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
}

// Storage configures the store.
type Storage struct {
	Type           string    `yaml:"type"`
	Path           string    `yaml:"path"`
	AuditRetention *Duration `yaml:"auditRetention"`
}

// Audit configures the audit stream.
type Audit struct {
	// Writer names the hash chain this process appends to. Processes sharing a store
	// and a writer share one chain.
	Writer  string    `yaml:"writer"`
	Stream  Stream    `yaml:"stream"`
	Timeout *Duration `yaml:"timeout"`
}

// Stream is where audit records are streamed besides the store.
type Stream struct {
	Type string `yaml:"type"` // stderr, stdout or file
	Path string `yaml:"path"`
}

// Providers configures backends.
type Providers struct {
	Kubernetes Kubernetes `yaml:"kubernetes"`
}

// Kubernetes lists clusters and their credential profiles.
type Kubernetes struct {
	Clusters map[string]Cluster `yaml:"clusters"`
}

// Cluster holds one credential per profile.
type Cluster struct {
	Profiles map[string]Profile `yaml:"profiles"`
}

// Profile references a kubeconfig; credentials are never inline.
type Profile struct {
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
}

// Environment is a named target.
type Environment struct {
	Name       string   `yaml:"name"`
	Cluster    string   `yaml:"cluster"`
	Namespaces []string `yaml:"namespaces"`
	Change     Change   `yaml:"change"`
}

// Change selects how changes reach an environment.
type Change struct {
	Strategy string `yaml:"strategy"`
}

// Load reads, decodes, defaults and validates the file at p.
func Load(p string) (*Config, error) {
	b, err := os.ReadFile(p) //nolint:gosec // G304: reading the operator's own configuration is the point
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b)
}

// Parse decodes, defaults and validates a configuration document. Unknown fields are
// errors, so a misspelled key cannot silently weaken policy.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Identity.Mode == "" {
		c.Identity.Mode = "local"
	}
	if c.Storage.Type == "" {
		c.Storage.Type = "sqlite"
	}
	if c.Storage.AuditRetention == nil {
		d := Duration(DefaultAuditRetention)
		c.Storage.AuditRetention = &d
	}
	if c.Audit.Stream.Type == "" {
		c.Audit.Stream.Type = "stderr"
	}
	if c.Audit.Timeout == nil {
		d := Duration(DefaultAuditTimeout)
		c.Audit.Timeout = &d
	}
	if c.Audit.Writer == "" {
		c.Audit.Writer = DefaultAuditWriter
	}
	if len(c.Policy.Sources) == 0 {
		c.Policy.Sources = []string{"file"}
	}
	for i := range c.Environments {
		e := &c.Environments[i]
		if len(e.Namespaces) == 0 {
			e.Namespaces = []string{"*"}
		}
		if e.Change.Strategy == "" {
			e.Change.Strategy = "suggest"
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Identity.Mode != "local" {
		fail("identity.mode: %q is not available in this version; use local", c.Identity.Mode)
	}
	if c.Identity.Principal.Subject == "" {
		fail("identity.principal.subject is required in local mode")
	}

	if c.Storage.Type != "sqlite" {
		fail("storage.type: %q is not available in this version; use sqlite", c.Storage.Type)
	}
	if c.Storage.Path == "" {
		fail("storage.path is required")
	}
	if *c.Storage.AuditRetention < 0 {
		fail("storage.auditRetention must not be negative")
	}

	switch c.Audit.Stream.Type {
	case "stderr", "stdout":
	case "file":
		if c.Audit.Stream.Path == "" {
			fail("audit.stream.path is required for type file")
		}
	default:
		fail("audit.stream.type: unknown type %q", c.Audit.Stream.Type)
	}
	if *c.Audit.Timeout <= 0 {
		fail("audit.timeout must be positive")
	}

	for name, cl := range c.Providers.Kubernetes.Clusters {
		if len(cl.Profiles) == 0 {
			fail("cluster %q: at least one profile is required", name)
		}
		for pn, p := range cl.Profiles {
			switch {
			case p.Kubeconfig == "":
				fail("cluster %q profile %q: kubeconfig is required", name, pn)
			case strings.HasPrefix(p.Kubeconfig, "~"):
				fail("cluster %q profile %q: kubeconfig %q: ~ is not expanded, use an absolute path", name, pn, p.Kubeconfig)
			}
		}
	}

	seen := map[string]bool{}
	for i, e := range c.Environments {
		if e.Name == "" {
			fail("environments[%d]: name is required", i)
			continue
		}
		if seen[e.Name] {
			fail("environment %q: duplicate name", e.Name)
		}
		seen[e.Name] = true
		if _, ok := c.Providers.Kubernetes.Clusters[e.Cluster]; !ok {
			fail("environment %q: unknown cluster %q", e.Name, e.Cluster)
		}
		for _, ns := range e.Namespaces {
			if _, err := path.Match(ns, ""); err != nil {
				fail("environment %q: invalid namespace pattern %q", e.Name, ns)
			}
		}
		if e.Change.Strategy != "suggest" {
			fail("environment %q: change strategy %q is not available in this version; use suggest", e.Name, e.Change.Strategy)
		}
	}
	return errors.Join(errs...)
}

// Profiles returns the profile names declared for an environment's cluster.
func (c *Config) Profiles(env Environment) []string {
	var out []string
	for name := range c.Providers.Kubernetes.Clusters[env.Cluster].Profiles {
		out = append(out, name)
	}
	return out
}

// Warnings describes configuration that is valid but probably not what was meant.
func (c *Config) Warnings() []string {
	var out []string
	if !filepath.IsAbs(c.Storage.Path) {
		out = append(out, fmt.Sprintf("storage.path %q is relative: it resolves against the working directory of whatever starts opsward, such as an MCP client", c.Storage.Path))
	}
	if c.Audit.Stream.Type == "stdout" {
		out = append(out, "audit.stream.type stdout cannot be used by opsward serve, which speaks MCP on stdout")
	}
	for name, cl := range c.Providers.Kubernetes.Clusters {
		for pn, p := range cl.Profiles {
			if p.Kubeconfig == "" || strings.HasPrefix(p.Kubeconfig, "~") {
				continue // already a validation error
			}
			if !filepath.IsAbs(p.Kubeconfig) {
				out = append(out, fmt.Sprintf("cluster %q profile %q: kubeconfig %q is relative", name, pn, p.Kubeconfig))
			}
			if _, err := os.Stat(p.Kubeconfig); err != nil {
				out = append(out, fmt.Sprintf("cluster %q profile %q: kubeconfig %q does not exist here; calls with this profile will fail until it does", name, pn, p.Kubeconfig))
			}
		}
	}
	sort.Strings(out)
	return out
}
