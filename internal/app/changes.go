package app

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/provider/forge"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
	"github.com/quyendv/opsward/internal/source"
	"github.com/quyendv/opsward/internal/tool"
)

// changeRoutes hands the change tools their configuration (docs/design/0008). Forges and
// the delivery reader are fixed until a restart; strategies and source resolution follow
// reloads.
type changeRoutes struct {
	forges []tool.ForgeRoute
	apps   func(ctx context.Context, namespace, name string) (*source.Application, error)
	appNS  string
	appNSs []string
	labels bool
	cur    atomic.Pointer[tool.Changes]
}

// openChanges builds the forges; it returns nil when none is configured, and the change
// tools are then not registered.
func openChanges(cfg *config.Config, k8s *kubernetes.Source) (*changeRoutes, error) {
	if len(cfg.Providers.Forges) == 0 {
		return nil, nil
	}
	c := &changeRoutes{}
	for i, f := range cfg.Providers.Forges {
		field := fmt.Sprintf("providers.forges[%d].token", i)
		token := f.Token
		fg, err := forge.New(forge.Options{Type: f.Type, URL: f.URL, Token: func() (string, error) { return token.Resolve(field) }})
		if err != nil {
			return nil, fmt.Errorf("forge %q: %w", f.Name, err)
		}
		c.forges = append(c.forges, tool.ForgeRoute{Name: f.Name, Hosts: f.Hosts, Repositories: f.Repositories, Forge: fg})
	}
	if d := cfg.Providers.Delivery; d != nil {
		cluster, profile := d.Cluster, d.Profile
		c.appNS, c.appNSs, c.labels = d.Namespace, d.ApplicationNamespaces, d.LabelTracking
		// The reader is looked up per call, so a reloaded profile is used once it changes.
		c.apps = func(ctx context.Context, namespace, name string) (*source.Application, error) {
			read, err := k8s.Applications(cluster, profile)
			if err != nil {
				return nil, err
			}
			return read(ctx, namespace, name)
		}
	}
	c.update(cfg)
	return c, nil
}

func (c *changeRoutes) Changes() *tool.Changes { return c.cur.Load() }

func (c *changeRoutes) update(cfg *config.Config) {
	ch := &tool.Changes{
		Strategies: map[string]string{}, Forges: c.forges,
		Resolve: source.Options{
			AnnotationKeys: cfg.SourceResolution.Annotations, Applications: c.apps, ApplicationNamespace: c.appNS,
			LabelTracking: c.labels, ApplicationNamespaces: c.appNSs,
		},
	}
	for _, e := range cfg.Environments {
		ch.Strategies[e.Name] = e.Change.Strategy
	}
	for _, s := range cfg.SourceResolution.Static {
		ch.Resolve.Static = append(ch.Resolve.Static, source.Static{
			Environment: s.Environment, Namespace: s.Namespace, Kind: s.Kind, Name: s.Name, Source: s.Source,
		})
	}
	c.cur.Store(ch)
}
