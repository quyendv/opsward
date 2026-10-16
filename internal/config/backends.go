package config

import "strings"

type backendKind int

const (
	kindMetrics backendKind = iota
	kindLogs
)

func (cl Cluster) own(k backendKind) *Backend {
	if k == kindMetrics {
		return cl.Metrics
	}
	return cl.Logs
}

func (c *Config) shared(k backendKind) *Backend {
	if k == kindMetrics {
		return c.Providers.Metrics
	}
	return c.Providers.Logs
}

// backends returns every configured backend by its field path.
func (c *Config) backends() map[string]*Backend {
	out := map[string]*Backend{}
	if c.Providers.Metrics != nil {
		out["providers.metrics"] = c.Providers.Metrics
	}
	if c.Providers.Logs != nil {
		out["providers.logs"] = c.Providers.Logs
	}
	for name, cl := range c.Providers.Kubernetes.Clusters {
		if cl.Metrics != nil {
			out["providers.kubernetes.clusters."+name+".metrics"] = cl.Metrics
		}
		if cl.Logs != nil {
			out["providers.kubernetes.clusters."+name+".logs"] = cl.Logs
		}
	}
	return out
}

// sharedClusters are the clusters, used by an environment, that the shared backend of a
// kind serves: those without their own.
func (c *Config) sharedClusters(k backendKind) map[string]bool {
	out := map[string]bool{}
	for _, e := range c.Environments {
		if c.Providers.Kubernetes.Clusters[e.Cluster].own(k) == nil {
			out[e.Cluster] = true
		}
	}
	return out
}

func (c *Config) backendFor(env string, k backendKind) (*Backend, bool) {
	for _, e := range c.Environments {
		if e.Name != env {
			continue
		}
		if b := c.Providers.Kubernetes.Clusters[e.Cluster].own(k); b != nil {
			return b, true
		}
		if b := c.shared(k); b != nil {
			return b, true
		}
	}
	return nil, false
}

// MetricsBackend returns the metrics backend an environment's cluster uses: its own, or
// the shared one.
func (c *Config) MetricsBackend(env string) (*Backend, bool) { return c.backendFor(env, kindMetrics) }

// LogsBackend returns the logs backend an environment's cluster uses.
func (c *Config) LogsBackend(env string) (*Backend, bool) { return c.backendFor(env, kindLogs) }

func (c *Config) kindBackends(k backendKind) map[string]*Backend {
	suffix := ".metrics"
	if k == kindLogs {
		suffix = ".logs"
	}
	out := map[string]*Backend{}
	for path, b := range c.backends() {
		if strings.HasSuffix(path, suffix) {
			out[path] = b
		}
	}
	return out
}

func (c *Config) backendPath(env string, k backendKind) string {
	suffix := ".metrics"
	if k == kindLogs {
		suffix = ".logs"
	}
	for _, e := range c.Environments {
		if e.Name != env {
			continue
		}
		if c.Providers.Kubernetes.Clusters[e.Cluster].own(k) != nil {
			return "providers.kubernetes.clusters." + e.Cluster + suffix
		}
		if c.shared(k) != nil {
			return "providers" + suffix
		}
	}
	return ""
}

// MetricsBackends returns every metrics backend by its field path.
func (c *Config) MetricsBackends() map[string]*Backend { return c.kindBackends(kindMetrics) }

// LogsBackends returns every logs backend by its field path.
func (c *Config) LogsBackends() map[string]*Backend { return c.kindBackends(kindLogs) }

// MetricsBackendPath names the metrics backend an environment uses, as MetricsBackends
// keys it; "" when it has none.
func (c *Config) MetricsBackendPath(env string) string { return c.backendPath(env, kindMetrics) }

// LogsBackendPath names the logs backend an environment uses; "" when it has none.
func (c *Config) LogsBackendPath(env string) string { return c.backendPath(env, kindLogs) }
