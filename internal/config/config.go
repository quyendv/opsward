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
	"regexp"
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
	Server       Server        `yaml:"server"`
	Storage      Storage       `yaml:"storage"`
	Audit        Audit         `yaml:"audit"`
	Providers    Providers     `yaml:"providers"`
	Environments []Environment `yaml:"environments"`
	Policy       policy.Config `yaml:"policy"`
	Knowledge    Knowledge     `yaml:"knowledge"`
	Approvals    Approvals     `yaml:"approvals"`
	// SourceResolution finds the files that declare a workload (docs/design/0008).
	SourceResolution SourceResolution `yaml:"sourceResolution"`
	// Alerts, when set, investigates Alertmanager's alerts (docs/design/0012).
	Alerts *Alerts `yaml:"alerts"`
	// Agent, when set, configures the model agent mode uses (docs/design/0013).
	Agent *Agent `yaml:"agent"`
}

// Approvals configures approval requests (docs/design/0007).
type Approvals struct {
	// TTL is how long a request can be decided and used; default one hour.
	TTL *Duration `yaml:"ttl"`
}

// Knowledge selects knowledge packs.
type Knowledge struct {
	Builtin *bool    `yaml:"builtin"` // the pack embedded in the binary; default true
	Paths   []string `yaml:"paths"`
}

// BuiltinEnabled reports whether the embedded pack is loaded.
func (k Knowledge) BuiltinEnabled() bool { return k.Builtin == nil || *k.Builtin }

// Identity configures how callers are identified. docs/design/0005.
type Identity struct {
	Mode      string    `yaml:"mode"`      // local (stdio, fixed principal) or remote (HTTP, tokens)
	Principal Principal `yaml:"principal"` // local mode only
	APITokens APITokens `yaml:"apiTokens"`
	OIDC      *OIDC     `yaml:"oidc"` // optional; remote mode
	// Groups are internal groups: group name to member subjects. Reloadable.
	Groups map[string][]string `yaml:"groups"`
}

// Principal is the fixed caller in local mode.
type Principal struct {
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
}

// Storage configures the store.
type Storage struct {
	Type           string     `yaml:"type"` // sqlite (default) or postgres
	Path           string     `yaml:"path"` // sqlite
	DSN            *SecretRef `yaml:"dsn"`  // postgres; may hold a password, so referenced
	AuditRetention *Duration  `yaml:"auditRetention"`
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
	Delivery   *Delivery  `yaml:"delivery"`
	Forges     []Forge    `yaml:"forges"`
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

// files maps each referenced file's field name to its path.
func (b *Backend) files() map[string]string {
	out := map[string]string{"auth.bearerTokenFile": b.Auth.BearerTokenFile, "caFile": b.CAFile}
	if b.Auth.Basic != nil {
		out["auth.basic.passwordFile"] = b.Auth.Basic.PasswordFile
	}
	return out
}

// Kubernetes lists clusters and their credential profiles.
type Kubernetes struct {
	Clusters map[string]Cluster `yaml:"clusters"`
}

// Cluster holds one credential per profile and, optionally, the cluster's own metrics
// and logs backends, used instead of the shared ones (docs/design/0006).
type Cluster struct {
	Profiles map[string]Profile `yaml:"profiles"`
	Metrics  *Backend           `yaml:"metrics"`
	Logs     *Backend           `yaml:"logs"`
}

// Profile references a kubeconfig; credentials are never inline.
type Profile struct {
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
	// Impersonate, if set, makes the profile act as the caller (docs/design/0014).
	Impersonate *Impersonate `yaml:"impersonate"`
}

// Impersonate names how a caller becomes a Kubernetes user and groups. A prefix left out
// is opsward:, so that principals stay apart from the cluster's own users; an empty one
// is kept.
type Impersonate struct {
	Username *string `yaml:"usernamePrefix"`
	Group    *string `yaml:"groupPrefix"`
}

const defaultImpersonationPrefix = "opsward:"

// UserPrefix is put before the principal's subject.
func (i *Impersonate) UserPrefix() string { return prefixOr(i.Username) }

// GroupPrefix is put before each group.
func (i *Impersonate) GroupPrefix() string { return prefixOr(i.Group) }

type prefix struct{ field, v string }

func (i *Impersonate) prefixes() []prefix {
	return []prefix{{"usernamePrefix", i.UserPrefix()}, {"groupPrefix", i.GroupPrefix()}}
}

func prefixOr(p *string) string {
	if p == nil {
		return defaultImpersonationPrefix
	}
	return *p
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
		c.Identity.Mode = ModeLocal
	}
	c.oidcDefaults()
	if c.Approvals.TTL == nil {
		d := Duration(time.Hour)
		c.Approvals.TTL = &d
	}
	if c.Identity.APITokens.MaxTTL == nil {
		d := Duration(DefaultAPITokenMaxTTL)
		c.Identity.APITokens.MaxTTL = &d
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
			e.Change.Strategy = StrategySuggest
		}
		if e.ClusterLabelValue == "" {
			e.ClusterLabelValue = e.Cluster
		}
	}
	c.changeDefaults()
	c.alertsDefaults()
	c.agentDefaults()
	for _, b := range c.backends() {
		if b.NamespaceLabel == "" {
			b.NamespaceLabel = "namespace"
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.validateIdentity(fail)

	c.validateStorage(fail)
	if d := c.Approvals.TTL.Std(); d <= 0 || d > 24*time.Hour {
		fail("approvals.ttl must be positive and at most 24h")
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
			if !dnsLabel.MatchString(pn) {
				fail("cluster %q profile %q: the name must be a DNS-1123 label (lowercase letters, digits and -)", name, pn)
			}
			switch {
			case p.Kubeconfig == "":
				fail("cluster %q profile %q: kubeconfig is required", name, pn)
			case strings.HasPrefix(p.Kubeconfig, "~"):
				fail("cluster %q profile %q: kubeconfig %q: ~ is not expanded, use an absolute path", name, pn, p.Kubeconfig)
			}
			if i := p.Impersonate; i != nil {
				for _, f := range i.prefixes() {
					if strings.HasPrefix(f.v, "system:") {
						fail("cluster %q profile %q: impersonate.%s %q: system: names are the cluster's own", name, pn, f.field, f.v)
					}
				}
			}
		}
	}

	c.validateBackend("providers.metrics", c.Providers.Metrics, "prometheus", c.sharedClusters(kindMetrics), fail)
	c.validateBackend("providers.logs", c.Providers.Logs, "loki", c.sharedClusters(kindLogs), fail)
	for name, cl := range c.Providers.Kubernetes.Clusters {
		c.validateBackend("providers.kubernetes.clusters."+name+".metrics", cl.Metrics, "prometheus", map[string]bool{name: true}, fail)
		c.validateBackend("providers.kubernetes.clusters."+name+".logs", cl.Logs, "loki", map[string]bool{name: true}, fail)
	}

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
		switch e.Change.Strategy {
		case StrategySuggest:
		case StrategyPullRequest:
			if len(c.Providers.Forges) == 0 {
				fail("environment %q: change strategy pull-request needs a forge in providers.forges", e.Name)
			}
		default:
			fail("environment %q: change strategy %q is not available; use suggest or pull-request", e.Name, e.Change.Strategy)
		}
	}
	c.validateChange(seen, fail)
	c.validateAlerts(fail)
	c.validateAgent(fail)
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
			if i := p.Impersonate; i != nil {
				for _, f := range i.prefixes() {
					if f.v == "" {
						out = append(out, fmt.Sprintf("cluster %q profile %q: impersonate.%s is empty: callers reach the cluster under its own names, such as an existing administrator's", name, pn, f.field))
					}
				}
			}
			if _, err := os.Stat(p.Kubeconfig); err != nil {
				out = append(out, fmt.Sprintf("cluster %q profile %q: kubeconfig %q does not exist here; calls with this profile will fail until it does", name, pn, p.Kubeconfig))
			}
		}
	}
	labelled := false
	for _, b := range c.backends() {
		labelled = labelled || b.ClusterLabel != ""
	}
	for _, e := range c.Environments {
		if !labelled && e.ClusterLabelValue != e.Cluster {
			out = append(out, fmt.Sprintf("environment %q: clusterLabelValue is ignored, no backend sets clusterLabel", e.Name))
		}
	}
	for name, b := range c.backends() {
		if strings.HasPrefix(b.URL, "http://") && (b.Auth.BearerTokenFile != "" || b.Auth.Basic != nil) {
			out = append(out, fmt.Sprintf("%s sends credentials over plain http", name))
		}
		// Warnings are logged on every reload: they name the field, not the path, which
		// for a credential file is better kept out of logs.
		for field, f := range b.files() {
			if f == "" || strings.HasPrefix(f, "~") {
				continue
			}
			if _, err := os.Stat(f); err != nil {
				out = append(out, fmt.Sprintf("%s.%s does not exist here", name, field))
			}
		}
	}
	for _, p := range c.Knowledge.Paths {
		if p != "" && !strings.HasPrefix(p, "~") && !filepath.IsAbs(p) {
			out = append(out, fmt.Sprintf("knowledge path %q is relative", p))
		}
	}
	for _, f := range c.Providers.Forges {
		if strings.HasPrefix(f.URL, "http://") {
			out = append(out, fmt.Sprintf("forge %q sends its token over plain http", f.Name))
		}
		if f.Token.File != "" {
			if _, err := os.Stat(f.Token.File); err != nil {
				out = append(out, fmt.Sprintf("forge %q: token.file does not exist here", f.Name))
			}
		}
	}
	out = append(out, c.remoteWarnings()...)
	out = append(out, c.alertWarnings()...)
	out = append(out, c.agentWarnings()...)
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
	if !reflect.DeepEqual(c.Server, next.Server) {
		out = append(out, "server")
	}
	if !reflect.DeepEqual(c.Storage, next.Storage) {
		out = append(out, "storage")
	}
	if !reflect.DeepEqual(c.Approvals, next.Approvals) {
		out = append(out, "approvals")
	}
	if !reflect.DeepEqual(c.Alerts, next.Alerts) {
		out = append(out, "alerts")
	}
	if !reflect.DeepEqual(c.Agent, next.Agent) {
		out = append(out, "agent")
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
	if !reflect.DeepEqual(c.Providers.Delivery, next.Providers.Delivery) {
		out = append(out, "providers.delivery")
	}
	if !reflect.DeepEqual(c.Providers.Forges, next.Providers.Forges) {
		out = append(out, "providers.forges")
	}
	for name, cl := range c.Providers.Kubernetes.Clusters {
		other := next.Providers.Kubernetes.Clusters[name]
		if !reflect.DeepEqual(cl.Metrics, other.Metrics) {
			out = append(out, "providers.kubernetes.clusters."+name+".metrics")
		}
		if !reflect.DeepEqual(cl.Logs, other.Logs) {
			out = append(out, "providers.kubernetes.clusters."+name+".logs")
		}
	}
	for name, cl := range next.Providers.Kubernetes.Clusters {
		if _, ok := c.Providers.Kubernetes.Clusters[name]; !ok && (cl.Metrics != nil || cl.Logs != nil) {
			out = append(out, "providers.kubernetes.clusters."+name)
		}
	}
	sort.Strings(out)
	return out
}

// dnsLabel is a DNS-1123 label: profile names become Kubernetes object names
// (docs/design/0009).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// labelName is the label syntax both Prometheus and Loki accept unquoted.
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (c *Config) validateBackend(name string, b *Backend, wantType string, serves map[string]bool, fail func(string, ...any)) {
	if b == nil {
		return
	}
	if b.Type != wantType {
		fail("%s.type: %q is not available; use %s", name, b.Type, wantType)
	}
	if b.URL == "" {
		fail("%s.url is required", name)
	} else if u, err := url.Parse(b.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fail("%s.url %q must be an http or https URL", name, b.URL)
	} else {
		if u.User != nil {
			// Not echoed: it may hold a password. Credentials are referenced by file.
			fail("%s.url must not contain credentials; use auth with a file", name)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			fail("%s.url must not have a query or fragment", name)
		}
	}
	if b.ClusterLabel == "" && len(serves) > 1 {
		fail("%s.clusterLabel is required when it serves more than one cluster: without it a query cannot be limited to one cluster", name)
	}
	for field, label := range map[string]string{"namespaceLabel": b.NamespaceLabel, "clusterLabel": b.ClusterLabel} {
		if label != "" && !labelName.MatchString(label) {
			fail("%s.%s: %q is not a valid label name ([a-zA-Z_][a-zA-Z0-9_]*)", name, field, label)
		}
	}
	if b.Auth.BearerTokenFile != "" && b.Auth.Basic != nil {
		fail("%s.auth: set one of bearerTokenFile and basic", name)
	}
	if b.Auth.Basic != nil && (b.Auth.Basic.Username == "" || b.Auth.Basic.PasswordFile == "") {
		fail("%s.auth.basic: username and passwordFile is required", name)
	}
	for field, f := range b.files() {
		if strings.HasPrefix(f, "~") {
			fail("%s.%s: ~ is not expanded, use an absolute path", name, field)
		}
	}
}

// normalIdentity makes equivalent identities compare equal: groups are a set, and an
// empty list is the same as an omitted one. Internal groups are left out, because a
// reload applies them, and so is apiTokens, which only the token commands read.
func normalIdentity(i Identity) Identity {
	i.Groups = nil
	i.APITokens = APITokens{}
	g := slices.Clone(i.Principal.Groups)
	slices.Sort(g)
	g = slices.Compact(g)
	if len(g) == 0 {
		g = nil
	}
	i.Principal.Groups = g
	return i
}
