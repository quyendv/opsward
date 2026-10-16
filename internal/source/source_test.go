package source

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func apps(m map[string]string) func(context.Context, string, string) (*Application, error) {
	return func(_ context.Context, ns, name string) (*Application, error) {
		raw, ok := m[ns+"/"+name]
		if !ok {
			return nil, ErrNoApplication
		}
		var a Application
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		return &a, nil
	}
}

var api = Workload{Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "api"}

func TestArgoCDSingleSource(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api","targetRevision":"main","helm":{"valueFiles":["values.yaml","values-staging.yaml"]}},"destination":{"namespace":"shop"}}}`,
	})})
	if err != nil {
		t.Fatal(err)
	}
	want := &Source{Resolver: "argocd", Application: "argocd/api", Locations: []Location{{
		Repo: "https://github.com/example-org/gitops.git", Revision: "main", Path: "apps/api",
		Files: []string{"apps/api/values.yaml", "apps/api/values-staging.yaml"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// A chart from a Helm repository with value files from a Git repository, through a ref.
func TestArgoCDMultiSourceValuesRef(t *testing.T) {
	w := api
	w.Labels = map[string]string{"app.kubernetes.io/instance": "team-a_api"} // label tracking, app in another namespace
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: apps(map[string]string{
		"team-a/api": `{"spec":{"sources":[
			{"repoURL":"https://charts.example.com","chart":"generic-app","targetRevision":"1.12.0","helm":{"valueFiles":["$values/workloads/api/values.yaml"]}},
			{"repoURL":"git@github.com:example-org/gitops.git","targetRevision":"HEAD","ref":"values"}],"destination":{"namespace":"shop"}}}`,
	}), LabelTracking: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Application != "team-a/api" || len(got.Locations) != 1 {
		t.Fatalf("got %+v", got)
	}
	l := got.Locations[0]
	if l.Repo != "git@github.com:example-org/gitops.git" || l.Revision != "HEAD" || l.Path != "" || !reflect.DeepEqual(l.Files, []string{"workloads/api/values.yaml"}) {
		t.Fatalf("location = %+v", l)
	}
}

func TestAnnotationAndStatic(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"opsward/source": "github:example-org/gitops//apps/api@release"}
	got, err := Resolve(context.Background(), w, Options{AnnotationKeys: []string{"example.com/source", "opsward/source"}})
	if err != nil || got.Resolver != "annotation" || !reflect.DeepEqual(got.Locations, []Location{{Repo: "github:example-org/gitops", Revision: "release", Path: "apps/api"}}) {
		t.Fatalf("annotation: %+v, %v", got, err)
	}
	got, err = Resolve(context.Background(), api, Options{Static: []Static{
		{Environment: "staging", Namespace: "shop", Name: "web", Source: "github:x/y//web"},
		{Environment: "staging", Namespace: "shop", Name: "api", Source: "https://gitlab.example.com/team/ops.git//api"},
	}})
	if err != nil || got.Resolver != "static" || got.Locations[0].Repo != "https://gitlab.example.com/team/ops.git" || got.Locations[0].Path != "api" {
		t.Fatalf("static: %+v, %v", got, err)
	}
}

func TestUnresolved(t *testing.T) {
	_, err := Resolve(context.Background(), api, Options{})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v", err)
	}
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "gone:apps/Deployment:shop/api"}
	_, err = Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: apps(nil)})
	if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("missing application: %v", err)
	}
}

func TestLocationContains(t *testing.T) {
	dir := Location{Repo: "r", Path: "apps/api"}
	files := Location{Repo: "r", Files: []string{"workloads/api/values.yaml"}}
	for _, tt := range []struct {
		l    Location
		path string
		want bool
	}{
		{dir, "apps/api/values.yaml", true},
		{dir, "apps/api/templates/deploy.yaml", true},
		{dir, "apps/api-other/values.yaml", false},
		{dir, "apps/api/../web/values.yaml", false},
		{dir, "/apps/api/values.yaml", false},
		{dir, "../apps/api/values.yaml", false},
		{dir, "apps/api/./values.yaml", false},
		{Location{Repo: "r", Path: "../.."}, "../../other/x", false},
		{Location{Repo: "r", Path: "."}, "values.yaml", true},
		{Location{Repo: "r", Path: "."}, "../values.yaml", false},
		{files, "workloads/api/values.yaml", true},
		{files, "workloads/api/other.yaml", false},
	} {
		if got := tt.l.Contains(tt.path); got != tt.want {
			t.Errorf("%+v contains %q = %v", tt.l, tt.path, got)
		}
	}
}

func TestParseRepository(t *testing.T) {
	for in, want := range map[string]Repository{
		"https://github.com/example-org/gitops.git": {Host: "github.com", Path: "example-org/gitops"},
		"https://github.com/example-org/gitops":     {Host: "github.com", Path: "example-org/gitops"},
		"git@github.com:example-org/gitops.git":     {Host: "github.com", Path: "example-org/gitops"},
		"ssh://git@git.example.com:2222/team/ops":   {Host: "git.example.com", Path: "team/ops"},
		"https://gitlab.example.com/group/sub/repo": {Host: "gitlab.example.com", Path: "group/sub/repo"},
		"github:example-org/gitops":                 {Forge: "github", Path: "example-org/gitops"},
	} {
		if got, err := ParseRepository(in); err != nil || got != want {
			t.Errorf("ParseRepository(%q) = %+v, %v", in, got, err)
		}
	}
}

// app.kubernetes.io/instance is also a chart convention: a label naming no Application
// falls through to the other resolvers.
func TestInstanceLabelWithoutApplicationFallsThrough(t *testing.T) {
	w := api
	w.Labels = map[string]string{"app.kubernetes.io/instance": "api"}
	got, err := Resolve(context.Background(), w, Options{
		ApplicationNamespace: "argocd", Applications: apps(nil), LabelTracking: true,
		Static: []Static{{Environment: "staging", Namespace: "shop", Name: "api", Source: "github:x/y//api"}},
	})
	if err != nil || got.Resolver != "static" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseLocationRefusesTraversal(t *testing.T) {
	for _, bad := range []string{
		"github:example-org/gitops//../../other-org/private",
		"github:example-org/gitops//apps/../../x",
		"github:example-org/gitops//apps@../../../../other-org/x/git/trees/main",
		"github:example-org/gitops//apps@-x",
		"github:example-org/gitops//apps@a..b",
		"github:example-org/gitops//apps@main?x=1",
	} {
		if l, err := ParseLocation(bad); err == nil {
			t.Errorf("ParseLocation(%q) = %+v, want an error", bad, l)
		}
	}
	for in, want := range map[string]Location{
		"github:o/r//./apps/api/@release/1.0": {Repo: "github:o/r", Path: "apps/api", Revision: "release/1.0"},
		"github:o/r//.":                       {Repo: "github:o/r", Path: "."},
	} {
		if got, err := ParseLocation(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseLocation(%q) = %+v, %v", in, got, err)
		}
	}
}

// Argo CD Applications often use "." or "./dir" as a path.
func TestArgoCDPathsAreCleaned(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/api.git","path":".","helm":{"valueFiles":["./values.yaml"]}},"destination":{"namespace":"shop"}}}`,
	})})
	if err != nil || got.Locations[0].Path != "." || !got.Locations[0].Contains("values.yaml") || got.Locations[0].Files[0] != "values.yaml" {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/api.git","path":"../other","targetRevision":"main"},"destination":{"namespace":"shop"}}}`,
	})})
	if err == nil {
		t.Fatal("an Application path outside the repository was accepted")
	}
}

// Whoever can annotate a workload must not be able to borrow another Application's
// source: the tracking id must name the workload, and the Application must deploy to its
// namespace, as Argo CD itself requires.
func TestTrackingIDMustNameTheWorkload(t *testing.T) {
	other := apps(map[string]string{
		"argocd/payments": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/payments"},"destination":{"namespace":"payments"}}}`,
	})
	for _, id := range []string{
		"payments:apps/Deployment:shop/other",
		"payments:apps/StatefulSet:shop/api",
		"payments:apps/Deployment:payments/api",
		"payments",
	} {
		w := api
		w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": id}
		if got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: other}); err == nil {
			t.Errorf("tracking id %q resolved to %+v", id, got)
		}
	}
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "payments:apps/Deployment:shop/api"}
	_, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Applications: other})
	if err == nil || !strings.Contains(err.Error(), "deploys to namespace payments") {
		t.Fatalf("an Application deploying elsewhere: err = %v", err)
	}
}

// The instance label is a chart convention as much as Argo CD's: it is read only when
// the configuration says Argo CD tracks by label.
func TestInstanceLabelNeedsLabelTracking(t *testing.T) {
	called := false
	w := api
	w.Labels = map[string]string{"app.kubernetes.io/instance": "api"}
	got, err := Resolve(context.Background(), w, Options{
		ApplicationNamespace: "argocd",
		Applications: func(context.Context, string, string) (*Application, error) {
			called = true
			return nil, ErrNoApplication
		},
		Static: []Static{{Environment: "staging", Namespace: "shop", Name: "api", Source: "github:x/y//api"}},
	})
	if err != nil || got.Resolver != "static" || called {
		t.Fatalf("got %+v, %v, application read %v", got, err, called)
	}
}
