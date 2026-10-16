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
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api","targetRevision":"main","helm":{"valueFiles":["values.yaml","values-staging.yaml"]}}}}`,
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
			{"repoURL":"git@github.com:example-org/gitops.git","targetRevision":"HEAD","ref":"values"}]}}`,
	})})
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
		ApplicationNamespace: "argocd", Applications: apps(nil),
		Static: []Static{{Environment: "staging", Namespace: "shop", Name: "api", Source: "github:x/y//api"}},
	})
	if err != nil || got.Resolver != "static" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
