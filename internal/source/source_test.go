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

// inCluster accepts, for staging, the cluster Argo CD runs in.
var inCluster = map[string][]string{"staging": {"https://kubernetes.default.svc", "in-cluster"}}

var api = Workload{Environment: "staging", Namespace: "shop", Kind: "Deployment", Name: "api"}

func TestArgoCDSingleSource(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api","targetRevision":"main","helm":{"valueFiles":["values.yaml","values-staging.yaml"]}},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}}}`,
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
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(map[string]string{
		"team-a/api": `{"spec":{"sources":[
			{"repoURL":"https://charts.example.com","chart":"generic-app","targetRevision":"1.12.0","helm":{"valueFiles":["$values/workloads/api/values.yaml"]}},
			{"repoURL":"git@github.com:example-org/gitops.git","targetRevision":"HEAD","ref":"values"}],"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}}}`,
	}), LabelTracking: true, ApplicationNamespaces: []string{"team-a"}})
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
	_, err = Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(nil)})
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
		ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(nil), LabelTracking: true,
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
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/api.git","path":".","helm":{"valueFiles":["./values.yaml"]}},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}}}`,
	})})
	if err != nil || got.Locations[0].Path != "." || !got.Locations[0].Contains("values.yaml") || got.Locations[0].Files[0] != "values.yaml" {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/api.git","path":"../other","targetRevision":"main"},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}}}`,
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
		"argocd/payments": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/payments"},"destination":{"server":"https://kubernetes.default.svc","namespace":"payments"}}}`,
	})
	for _, id := range []string{
		"payments:apps/Deployment:shop/other",
		"payments:apps/StatefulSet:shop/api",
		"payments:apps/Deployment:payments/api",
		"payments",
	} {
		w := api
		w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": id}
		if got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: other}); err == nil {
			t.Errorf("tracking id %q resolved to %+v", id, got)
		}
	}
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "payments:apps/Deployment:shop/api"}
	_, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: other})
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
		ApplicationNamespace: "argocd", Destinations: inCluster,
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

// Applications outside the default namespace (<namespace>_<name>) are read only from the
// namespaces the configuration lists, as Argo CD's own setting does.
func TestApplicationNamespacesAreListed(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "team-a_api:apps/Deployment:shop/api"}
	read := apps(map[string]string{
		"team-a/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api"},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}}}`,
	})
	if _, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: read}); !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "team-a") {
		t.Fatalf("an unlisted namespace: err = %v", err)
	}
	got, err := Resolve(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, ApplicationNamespaces: []string{"team-a"}, Applications: read})
	if err != nil || got.Application != "team-a/api" {
		t.Fatalf("a listed namespace: %+v, %v", got, err)
	}
}

// Managing finds the Application that manages a workload with the same checks as source
// resolution, and nothing when nothing tracks the workload.
func TestManaging(t *testing.T) {
	read := apps(map[string]string{
		"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api","targetRevision":"main"},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
			"status":{"sync":{"status":"OutOfSync","revision":"abc123"},"health":{"status":"Degraded","message":"1 pod crashing"},
			"operationState":{"phase":"Failed","message":"one or more objects failed to apply","startedAt":"2026-10-16T08:00:00Z","finishedAt":"2026-10-16T08:00:05Z","syncResult":{"revision":"abc123"}},
			"conditions":[{"type":"SyncError","message":"Failed sync attempt"}],
			"resources":[{"group":"apps","kind":"Deployment","namespace":"shop","name":"api","status":"OutOfSync","health":{"status":"Degraded"}},{"kind":"Service","namespace":"shop","name":"api","status":"Synced","health":{"status":"Healthy"}}],
			"history":[{"revision":"aaa111","deployedAt":"2026-10-15T08:00:00Z"},{"revision":"abc123","deployedAt":"2026-10-16T08:00:05Z"}]}}`,
	})
	opt := Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: read}
	if m, err := Managing(context.Background(), api, opt); m != nil || err != nil {
		t.Fatalf("untracked workload: %+v, %v", m, err)
	}
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}
	m, err := Managing(context.Background(), w, opt)
	if err != nil || m.Namespace != "argocd" || m.Name != "api" {
		t.Fatalf("Managing = %+v, %v", m, err)
	}
	st := m.App.Status
	if st.Sync.Status != "OutOfSync" || st.Health.Message != "1 pod crashing" || st.OperationState.Phase != "Failed" ||
		len(st.Conditions) != 1 || len(st.Resources) != 2 || st.History[1].Revision != "abc123" {
		t.Fatalf("status = %+v", st)
	}
	w.Annotations["argocd.argoproj.io/tracking-id"] = "api:apps/Deployment:shop/other"
	if _, err := Managing(context.Background(), w, opt); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("a tracking id naming another workload: err = %v", err)
	}
}

// An Application deploying to another cluster does not manage a workload of this
// environment, even under the same namespace and name: environments on clusters Argo CD
// does not reach list their destination.
func TestApplicationMustDeployToTheEnvironmentsCluster(t *testing.T) {
	w := api
	w.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}
	for _, dest := range []string{`{"server":"https://cluster-b.example.com","namespace":"shop"}`, `{"name":"cluster-b","namespace":"shop"}`, `{"namespace":"shop"}`} {
		read := apps(map[string]string{"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api"},"destination":` + dest + `}}`})
		if m, err := Managing(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: inCluster, Applications: read}); !errors.Is(err, ErrUnresolved) {
			t.Errorf("destination %s: %+v, %v", dest, m, err)
		}
	}
	read := apps(map[string]string{"argocd/api": `{"spec":{"source":{"repoURL":"https://github.com/example-org/gitops.git","path":"apps/api"},"destination":{"name":"cluster-b","namespace":"shop"}}}`})
	dests := map[string][]string{"staging": {"cluster-b"}}
	if m, err := Managing(context.Background(), w, Options{ApplicationNamespace: "argocd", Destinations: dests, Applications: read}); err != nil || m == nil {
		t.Fatalf("a listed destination: %+v, %v", m, err)
	}
}

// An environment can bound where its workloads' annotations may point.
func TestAnnotationPathsBoundAnnotations(t *testing.T) {
	w := api
	opt := Options{AnnotationKeys: []string{"opsward/source"}, AnnotationPaths: map[string][]string{"staging": {"apps/staging", "shared/values"}}}
	for path, ok := range map[string]bool{
		"apps/staging/api":    true,
		"shared/values":       true,
		"apps/staging-x/api":  false,
		"apps/production/api": false,
		".":                   false,
	} {
		w.Annotations = map[string]string{"opsward/source": "github:example-org/gitops//" + path}
		_, err := Resolve(context.Background(), w, opt)
		if ok && err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if !ok && (!errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "annotationPaths")) {
			t.Errorf("%s: err = %v, want refused", path, err)
		}
	}
	// Other environments, and the static map, are not bound.
	w.Environment = "production"
	w.Annotations = map[string]string{"opsward/source": "github:example-org/gitops//anywhere"}
	if _, err := Resolve(context.Background(), w, opt); err != nil {
		t.Fatal(err)
	}
}
