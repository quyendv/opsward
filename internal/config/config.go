// Package config loads and validates the Opsward configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
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
	Knowledge    Knowledge     `yaml:"knowledge"`
}

// Knowledge selects knowledge packs.
type Knowledge struct {
	Builtin *bool    `yaml:"builtin"` // the pack embedded in the binary; default true
	Paths   []string `yaml:"paths"`
}

// BuiltinEnabled reports whether the embedded pack is loaded.
func (k Knowledge) BuiltinEnabled() bool { return k.Builtin == nil || *k.Builtin }

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
	Metrics    *Backend   `yaml:"metrics"` // optional; restart-only (docs/design/0004)
	Logs       *Backend   `yaml:"logs"`    // optional; restart-only
}

// Backend is an HTTP query backend: Prometheus-compatible metrics or Loki logs.
type Backend struct {
	Type           string      `yaml:"type"`
	URL            string      `yaml:"url"`
	NamespaceLabel string      `yaml:"namespaceLabel"`
	ClusterLabel   string      `yaml:"clusterLabel"` // empty: one cluster, no matcher added
	Tenant         string      `yaml:"tenant"`       // sent as X-Scope-OrgID
	Auth           BackendAuth `yaml:"auth"`
	CAFile         string      `yaml:"caFile"`
}

// BackendAuth references credentials for a backend; at most one kind.
type BackendAuth struct {
	BearerTokenFile string     `yaml:"bearerTokenFile"`
	Basic           *BasicAuth `yaml:"basic"`
}

// BasicAuth references a username and a password file.
type BasicAuth struct {
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"passwordFile"`
}

func (b *Backend) files() []string {
	out := []string{b.Auth.BearerTokenFile, b.CAFile}
	if b.Auth.Basic != nil {
		out = append(out, b.Auth.Basic.PasswordFile)
	}
	return out
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
	// ClusterLabelValue is this environment's value of a backend's clusterLabel; default
	// the cluster name.
	ClusterLabelValue string `yaml:"clusterLabelValue"`
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
		if e.ClusterLabelValue == "" {
			e.ClusterLabelValue = e.Cluster
		}
	}
	for _, b := range []*Backend{c.Providers.Metrics, c.Providers.Logs} {
		if b != nil && b.NamespaceLabel == "" {
			b.NamespaceLabel = "namespace"
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

	c.validateBackend("metrics", c.Providers.Metrics, "prometheus", fail)
	c.validateBackend("logs", c.Providers.Logs, "loki", fail)

	for _, p := range c.Knowledge.Paths {
		switch {
		case p == "":
			fail("knowledge.paths: empty path")
		case strings.HasPrefix(p, "~"):
			fail("knowledge path %q: ~ is not expanded, use an absolute path", p)
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
	for name, b := range map[string]*Backend{"metrics": c.Providers.Metrics, "logs": c.Providers.Logs} {
		if b == nil {
			continue
		}
		if strings.HasPrefix(b.URL, "http://") && (b.Auth.BearerTokenFile != "" || b.Auth.Basic != nil) {
			out = append(out, fmt.Sprintf("providers.%s sends credentials over plain http", name))
		}
		for _, f := range b.files() {
			if f == "" || strings.HasPrefix(f, "~") {
				continue
			}
			if _, err := os.Stat(f); err != nil {
				out = append(out, fmt.Sprintf("providers.%s: %q does not exist here", name, f))
			}
		}
	}
	for _, p := range c.Knowledge.Paths {
		if p != "" && !strings.HasPrefix(p, "~") && !filepath.IsAbs(p) {
			out = append(out, fmt.Sprintf("knowledge path %q is relative", p))
		}
	}
	sort.Strings(out)
	return out
}

// RestartRequired lists the sections that differ between c and next and that a running
// process cannot apply: identity, storage and audit. docs/design/0003.
func (c *Config) RestartRequired(next *Config) []string {
	var out []string
	if !reflect.DeepEqual(normalIdentity(c.Identity), normalIdentity(next.Identity)) {
		out = append(out, "identity")
	}
	if !reflect.DeepEqual(c.Storage, next.Storage) {
		out = append(out, "storage")
	}
	if !reflect.DeepEqual(c.Audit, next.Audit) {
		out = append(out, "audit")
	}
	// Tools that need these backends are registered at start-up.
	if !reflect.DeepEqual(c.Providers.Metrics, next.Providers.Metrics) {
		out = append(out, "providers.metrics")
	}
	if !reflect.DeepEqual(c.Providers.Logs, next.Providers.Logs) {
		out = append(out, "providers.logs")
	}
	return out
}

func (c *Config) validateBackend(name string, b *Backend, wantType string, fail func(string, ...any)) {
	if b == nil {
		return
	}
	if b.Type != wantType {
		fail("providers.%s.type: %q is not available; use %s", name, b.Type, wantType)
	}
	if b.URL == "" {
		fail("providers.%s.url is required", name)
	} else if u, err := url.Parse(b.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fail("providers.%s.url %q must be an http or https URL", name, b.URL)
	}
	if b.Auth.BearerTokenFile != "" && b.Auth.Basic != nil {
		fail("providers.%s.auth: set one of bearerTokenFile and basic", name)
	}
	if b.Auth.Basic != nil && (b.Auth.Basic.Username == "" || b.Auth.Basic.PasswordFile == "") {
		fail("providers.%s.auth.basic: username and passwordFile is required", name)
	}
	for _, f := range b.files() {
		if strings.HasPrefix(f, "~") {
			fail("providers.%s: %q: ~ is not expanded, use an absolute path", name, f)
		}
	}
}

// normalIdentity makes equivalent identities compare equal: groups are a set, and an
// empty list is the same as an omitted one.
func normalIdentity(i Identity) Identity {
	g := slices.Clone(i.Principal.Groups)
	slices.Sort(g)
	g = slices.Compact(g)
	if len(g) == 0 {
		g = nil
	}
	i.Principal.Groups = g
	return i
}
