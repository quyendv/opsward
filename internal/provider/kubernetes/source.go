package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/quyendv/opsward/internal/capability"
)

// Profile is one credential: a kubeconfig file and, optionally, a context in it.
type Profile struct {
	Kubeconfig string
	Context    string
	// Impersonate, if set, makes the profile act as the caller of each request.
	Impersonate *Impersonation
}

// ClientFactory builds a client for a profile. Tests replace it.
type ClientFactory func(Profile) (kubernetes.Interface, error)

// Source hands out Workloads per environment and profile, building each client once,
// on first use, so a cluster that is unreachable does not stop start-up.
type Source struct {
	envCluster map[string]string
	profiles   map[string]map[string]Profile
	newClient  ClientFactory
	newDynamic DynamicFactory

	mu       sync.Mutex
	clients  map[string]cachedClient
	dynamics map[string]dynamic.Interface
}

// cachedClient remembers the kubeconfig content a client was built from, so that
// credentials rotated in place are noticed on the next Update.
type cachedClient struct {
	c   kubernetes.Interface
	sum string
}

var _ capability.WorkloadSource = (*Source)(nil)

// NewSource maps environments to clusters and clusters to profiles. A nil factory
// builds clients from kubeconfig files.
func NewSource(envCluster map[string]string, profiles map[string]map[string]Profile, f ClientFactory) *Source {
	if f == nil {
		f = FromKubeconfig
	}
	return &Source{
		envCluster: envCluster, profiles: profiles, newClient: f, newDynamic: DynamicFromKubeconfig,
		clients: map[string]cachedClient{}, dynamics: map[string]dynamic.Interface{},
	}
}

// Workloads implements capability.WorkloadSource. An unknown environment or a profile
// the cluster does not declare is an error, never a fallback to another credential.
func (s *Source) Workloads(environment, profile string) (capability.Workloads, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cluster, ok := s.envCluster[environment]
	if !ok {
		return nil, fmt.Errorf("kubernetes: unknown environment %q", environment)
	}
	p, ok := s.profiles[cluster][profile]
	if !ok {
		return nil, fmt.Errorf("kubernetes: cluster %q has no profile %q", cluster, profile)
	}
	key := cluster + "/" + profile
	cc, ok := s.clients[key]
	if !ok {
		c, err := s.newClient(p)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: cluster %q profile %q: %w", cluster, profile, err)
		}
		cc = cachedClient{c: c, sum: fileSum(p.Kubeconfig)}
		s.clients[key] = cc
	}
	return NewWorkloads(cc.c), nil
}

// Update replaces the environment and profile maps, as on a configuration reload. A
// client is kept only while its profile and its kubeconfig's content are unchanged;
// others are rebuilt on their next use.
func (s *Source) Update(envCluster map[string]string, profiles map[string]map[string]Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, cc := range s.clients {
		cluster, profile, _ := strings.Cut(key, "/")
		old, now := s.profiles[cluster][profile], profiles[cluster][profile]
		if !old.equal(now) || fileSum(now.Kubeconfig) != cc.sum {
			delete(s.clients, key)
		}
	}
	// Dynamic clients are rare and cheap to rebuild.
	s.dynamics = map[string]dynamic.Interface{}
	s.envCluster, s.profiles = envCluster, profiles
}

// fileSum returns a hash of the file's content, or "" if it cannot be read.
func fileSum(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's configured kubeconfig
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FromKubeconfig builds a client from a kubeconfig file (see restConfig).
func FromKubeconfig(p Profile) (kubernetes.Interface, error) {
	cfg, err := restConfig(p)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
