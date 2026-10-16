package config

import (
	"net/url"
	"strings"

	"github.com/quyendv/opsward/internal/source"
)

// Change strategies (docs/design/0008).
const (
	StrategySuggest     = "suggest"
	StrategyPullRequest = "pull-request"
)

// Delivery names where the reconciler's objects live; restart-only.
type Delivery struct {
	Type      string `yaml:"type"` // argocd
	Cluster   string `yaml:"cluster"`
	Namespace string `yaml:"namespace"` // default argocd
	Profile   string `yaml:"profile"`   // default reader
}

// Forge is a Git hosting service Opsward reads from and proposes changes to;
// restart-only.
type Forge struct {
	Name  string    `yaml:"name"`
	Type  string    `yaml:"type"` // github, gitea (also Forgejo) or gitlab
	URL   string    `yaml:"url"`  // the API base
	Hosts []string  `yaml:"hosts"`
	Token SecretRef `yaml:"token"`
	// Repositories are the only ones Opsward reads or writes on this forge.
	Repositories []string `yaml:"repositories"`
}

// SourceResolution configures how a workload's source is found.
type SourceResolution struct {
	// Annotations are workload annotation keys holding <repository>//<path>[@<revision>];
	// default [opsward/source].
	Annotations []string       `yaml:"annotations"`
	Static      []StaticSource `yaml:"static"`
}

// StaticSource maps one workload to its source; an empty kind matches any kind.
type StaticSource struct {
	Environment string `yaml:"environment"`
	Namespace   string `yaml:"namespace"`
	Kind        string `yaml:"kind"`
	Name        string `yaml:"name"`
	Source      string `yaml:"source"`
}

func (c *Config) changeDefaults() {
	if d := c.Providers.Delivery; d != nil {
		if d.Namespace == "" {
			d.Namespace = "argocd"
		}
		if d.Profile == "" {
			d.Profile = "reader"
		}
	}
	if c.SourceResolution.Annotations == nil {
		c.SourceResolution.Annotations = []string{"opsward/source"}
	}
}

func (c *Config) validateChange(envs map[string]bool, fail func(string, ...any)) {
	if d := c.Providers.Delivery; d != nil {
		if d.Type != "argocd" {
			fail("providers.delivery.type: %q is not available; use argocd", d.Type)
		}
		if cl, ok := c.Providers.Kubernetes.Clusters[d.Cluster]; !ok {
			fail("providers.delivery: unknown cluster %q", d.Cluster)
		} else if _, ok := cl.Profiles[d.Profile]; !ok {
			fail("providers.delivery: cluster %q has no profile %q", d.Cluster, d.Profile)
		}
	}
	names := map[string]bool{}
	for i, f := range c.Providers.Forges {
		if f.Name == "" {
			fail("providers.forges[%d]: name is required", i)
			continue
		}
		if names[f.Name] {
			fail("forge %q: duplicate name", f.Name)
		}
		names[f.Name] = true
		switch f.Type {
		case "github", "gitea", "gitlab":
		default:
			fail("forge %q: type %q is not available; use github, gitea or gitlab", f.Name, f.Type)
		}
		if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fail("forge %q: url must be an http or https URL", f.Name)
		} else if u.User != nil {
			fail("forge %q: url must not contain credentials; use token", f.Name)
		}
		if f.Token.File == "" && f.Token.Env == "" {
			fail("forge %q: token { file } or { env } is required", f.Name)
		}
		if len(f.Repositories) == 0 {
			fail("forge %q: repositories is required: Opsward reaches only the repositories a forge lists", f.Name)
		}
		for _, r := range f.Repositories {
			if r == "" || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") || !strings.Contains(r, "/") || strings.Contains(r, "//") {
				fail("forge %q: repository %q: want owner/name", f.Name, r)
			}
		}
	}
	for i, s := range c.SourceResolution.Static {
		if !envs[s.Environment] {
			fail("sourceResolution.static[%d]: unknown environment %q", i, s.Environment)
		}
		if s.Namespace == "" || s.Name == "" {
			fail("sourceResolution.static[%d]: namespace and name are required", i)
		}
		if _, err := source.ParseLocation(s.Source); err != nil {
			fail("sourceResolution.static[%d]: %v", i, err)
		}
	}
	for _, k := range c.SourceResolution.Annotations {
		if k == "" {
			fail("sourceResolution.annotations: empty key")
		}
	}
}
