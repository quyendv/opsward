package app

import (
	"fmt"
	"sync/atomic"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/provider/loki"
	"github.com/quyendv/opsward/internal/provider/prometheus"
)

// routes hands each environment the source of the backend its cluster uses: the
// cluster's own or the shared one (docs/design/0006). Sources are fixed until a restart;
// which environment uses which follows reloads.
type routes[S interface{ Update(map[string]string) }] struct {
	kind    string
	sources map[string]S // by backend path, as config keys it
	envs    atomic.Pointer[map[string]string]
}

func (r *routes[S]) source(env string) (S, error) {
	var zero S
	if m := r.envs.Load(); m != nil {
		if p, ok := (*m)[env]; ok {
			return r.sources[p], nil
		}
	}
	return zero, fmt.Errorf("%s: environment %q has no %s backend", r.kind, env, r.kind)
}

// update routes environments to backends (paths) and gives each source the cluster label
// values of the environments it serves.
func (r *routes[S]) update(paths, values map[string]string) {
	per := map[string]map[string]string{}
	for env, p := range paths {
		if per[p] == nil {
			per[p] = map[string]string{}
		}
		per[p][env] = values[env]
	}
	for p, src := range r.sources {
		src.Update(per[p])
	}
	r.envs.Store(&paths)
}

type metricsRoutes struct{ routes[*prometheus.Source] }

func (r *metricsRoutes) Metrics(env string) (capability.Metrics, error) {
	src, err := r.source(env)
	if err != nil {
		return nil, err
	}
	return src.Metrics(env)
}

type logsRoutes struct{ routes[*loki.Source] }

func (r *logsRoutes) Logs(env string) (capability.Logs, error) {
	src, err := r.source(env)
	if err != nil {
		return nil, err
	}
	return src.Logs(env)
}

// openBackends builds a source per configured backend; nil when there is none of a kind.
func openBackends(cfg *config.Config) (*metricsRoutes, *logsRoutes, error) {
	var mr *metricsRoutes
	var lr *logsRoutes
	if bs := cfg.MetricsBackends(); len(bs) > 0 {
		mr = &metricsRoutes{routes[*prometheus.Source]{kind: "metrics", sources: map[string]*prometheus.Source{}}}
		for path, b := range bs {
			src, err := prometheus.NewSource(prometheus.Options{HTTP: httpOptions(b), NamespaceLabel: b.NamespaceLabel, ClusterLabel: b.ClusterLabel}, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", path, err)
			}
			mr.sources[path] = src
		}
	}
	if bs := cfg.LogsBackends(); len(bs) > 0 {
		lr = &logsRoutes{routes[*loki.Source]{kind: "logs", sources: map[string]*loki.Source{}}}
		for path, b := range bs {
			src, err := loki.NewSource(loki.Options{HTTP: httpOptions(b), NamespaceLabel: b.NamespaceLabel, ClusterLabel: b.ClusterLabel}, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", path, err)
			}
			lr.sources[path] = src
		}
	}
	return mr, lr, nil
}
