package kubernetes

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/quyendv/opsward/internal/source"
)

func argoSource(t *testing.T, objs ...runtime.Object) *Source {
	t.Helper()
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{applicationsGVR: "ApplicationList"}, objs...)
	profiles := map[string]map[string]Profile{"cluster-a": {"reader": {Kubeconfig: "/nonexistent"}}}
	return NewSource(map[string]string{"staging": "cluster-a"}, profiles,
		func(Profile) (kubernetes.Interface, error) { return fake.NewClientset(), nil }).
		WithDynamic(func(Profile) (dynamic.Interface, error) { return dc, nil })
}

func TestApplicationsReadsTheSpec(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "api", "namespace": "argocd"},
		"spec": map[string]any{"source": map[string]any{
			"repoURL": "https://git.example.com/team-a/gitops.git", "path": "apps/api", "targetRevision": "main",
		}},
	}}
	read, err := argoSource(t, app).Applications("cluster-a", "reader")
	if err != nil {
		t.Fatal(err)
	}
	got, err := read(context.Background(), "argocd", "api")
	if err != nil || got.Spec.Source == nil || got.Spec.Source.Path != "apps/api" || got.Spec.Source.TargetRevision != "main" {
		t.Fatalf("Application = %+v, %v", got, err)
	}
	if _, err := read(context.Background(), "argocd", "web"); !errors.Is(err, source.ErrNoApplication) {
		t.Fatalf("missing Application: err = %v, want ErrNoApplication", err)
	}
}

func TestApplicationsNeedADeclaredProfile(t *testing.T) {
	if _, err := argoSource(t).Applications("cluster-a", "operator"); err == nil {
		t.Fatal("an undeclared profile was accepted")
	}
	if _, err := argoSource(t).Applications("cluster-b", "reader"); err == nil {
		t.Fatal("an unknown cluster was accepted")
	}
}
