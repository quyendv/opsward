package kubernetes

import (
	"fmt"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/version"
)

// Profile is one credential: a kubeconfig file and, optionally, a context in it.
type Profile struct {
	Kubeconfig string
	Context    string
}

// ClientFactory builds a client for a profile. Tests replace it.
type ClientFactory func(Profile) (kubernetes.Interface, error)

// Source hands out Workloads per environment and profile, building each client once,
// on first use, so a cluster that is unreachable does not stop start-up.
type Source struct {
	envCluster map[string]string
	profiles   map[string]map[string]Profile
	newClient  ClientFactory

	mu      sync.Mutex
	clients map[string]kubernetes.Interface
}

var _ capability.WorkloadSource = (*Source)(nil)

// NewSource maps environments to clusters and clusters to profiles. A nil factory
// builds clients from kubeconfig files.
func NewSource(envCluster map[string]string, profiles map[string]map[string]Profile, f ClientFactory) *Source {
	if f == nil {
		f = FromKubeconfig
	}
	return &Source{envCluster: envCluster, profiles: profiles, newClient: f, clients: map[string]kubernetes.Interface{}}
}

// Workloads implements capability.WorkloadSource. An unknown environment or a profile
// the cluster does not declare is an error, never a fallback to another credential.
func (s *Source) Workloads(environment, profile string) (capability.Workloads, error) {
	cluster, ok := s.envCluster[environment]
	if !ok {
		return nil, fmt.Errorf("kubernetes: unknown environment %q", environment)
	}
	p, ok := s.profiles[cluster][profile]
	if !ok {
		return nil, fmt.Errorf("kubernetes: cluster %q has no profile %q", cluster, profile)
	}
	key := cluster + "/" + profile
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[key]
	if !ok {
		var err error
		if c, err = s.newClient(p); err != nil {
			return nil, fmt.Errorf("kubernetes: cluster %q profile %q: %w", cluster, profile, err)
		}
		s.clients[key] = c
	}
	return NewWorkloads(c), nil
}

// FromKubeconfig builds a client from a kubeconfig file. It never falls back to
// in-cluster credentials, which client-go's deferred loader does for an empty file:
// a profile must not silently borrow the pod's own ServiceAccount.
func FromKubeconfig(p Profile) (kubernetes.Interface, error) {
	raw, err := clientcmd.LoadFromFile(p.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if len(raw.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig %s: no contexts", p.Kubeconfig)
	}
	if p.Context == "" && raw.CurrentContext == "" {
		return nil, fmt.Errorf("kubeconfig %s: no current-context; set the profile's context field", p.Kubeconfig)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, p.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", p.Kubeconfig, err)
	}
	cfg.UserAgent = "opsward/" + version.Version
	return kubernetes.NewForConfig(cfg)
}
