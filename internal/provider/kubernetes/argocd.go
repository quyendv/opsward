package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/quyendv/opsward/internal/source"
)

var applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// DynamicFactory builds a dynamic client for a profile. Tests replace it.
type DynamicFactory func(Profile) (dynamic.Interface, error)

// WithDynamic replaces the factory for dynamic clients, which read Argo CD Applications.
func (s *Source) WithDynamic(f DynamicFactory) *Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newDynamic = f
	s.dynamics = map[string]dynamic.Interface{}
	return s
}

// Applications returns a reader of Argo CD Applications in a cluster, with one of its
// declared profiles.
func (s *Source) Applications(cluster, profile string) (func(ctx context.Context, namespace, name string) (*source.Application, error), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[cluster][profile]
	if !ok {
		return nil, fmt.Errorf("kubernetes: cluster %q has no profile %q", cluster, profile)
	}
	key := cluster + "/" + profile
	dc, ok := s.dynamics[key]
	if !ok {
		var err error
		if dc, err = s.newDynamic(p); err != nil {
			return nil, fmt.Errorf("kubernetes: cluster %q profile %q: %w", cluster, profile, err)
		}
		s.dynamics[key] = dc
	}
	return func(ctx context.Context, namespace, name string) (*source.Application, error) {
		u, err := dc.Resource(applicationsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s/%s: %w", namespace, name, source.ErrNoApplication)
		}
		if err != nil {
			return nil, fmt.Errorf("get Application %s/%s: %w", namespace, name, err)
		}
		b, err := json.Marshal(u.Object)
		if err != nil {
			return nil, fmt.Errorf("decoding application %s/%s: %w", namespace, name, err)
		}
		var app source.Application
		if err := json.Unmarshal(b, &app); err != nil {
			return nil, fmt.Errorf("decoding application %s/%s: %w", namespace, name, err)
		}
		return &app, nil
	}, nil
}

// DynamicFromKubeconfig builds a dynamic client from a kubeconfig file (see restConfig).
func DynamicFromKubeconfig(p Profile) (dynamic.Interface, error) {
	cfg, err := restConfig(p)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}
