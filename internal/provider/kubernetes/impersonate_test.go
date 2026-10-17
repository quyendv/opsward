package kubernetes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/quyendv/opsward/internal/core"
)

// apiServer answers every request with 404 and records the impersonation headers it saw.
type apiServer struct {
	mu       sync.Mutex
	requests int
	users    []string
	groups   [][]string
}

func (s *apiServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.users = append(s.users, r.Header.Get("Impersonate-User"))
		s.groups = append(s.groups, r.Header.Values("Impersonate-Group"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeKubeconfig(t *testing.T, server, user string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kc := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "` + server + `"}}]
users: [{name: u, user: {token: placeholder` + user + `}}]
contexts: [{name: ctx, context: {cluster: c, user: u}}]
current-context: ctx
`
	if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func impersonating(kubeconfig string) Profile {
	return Profile{Kubeconfig: kubeconfig, Impersonate: &Impersonation{
		UserPrefix: "opsward:", GroupPrefix: "opsward:", Groups: []string{"team-a"},
	}}
}

func callerCtx(t *testing.T, p core.Principal) context.Context {
	t.Helper()
	return core.WithPrincipal(t.Context(), p)
}

func TestImpersonatingProfileActsAsTheCaller(t *testing.T) {
	var s apiServer
	c, err := FromKubeconfig(impersonating(writeKubeconfig(t, s.start(t), "")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := callerCtx(t, core.Principal{Subject: "Alice@Example.com", Groups: []string{"team-b", "team-a"}})
	_, _ = c.AppsV1().Deployments("team-a").Get(ctx, "web", metav1.GetOptions{})
	if s.requests != 1 {
		t.Fatalf("requests = %d, want 1", s.requests)
	}
	// The subject is normalized as policy matches it; only groups policy names for the
	// profile are sent, since one the credential may not impersonate fails the request.
	if s.users[0] != "opsward:alice@example.com" || !slices.Equal(s.groups[0], []string{"opsward:team-a"}) {
		t.Fatalf("impersonated user %q groups %q", s.users[0], s.groups[0])
	}
}

func TestImpersonatingDynamicClientActsAsTheCaller(t *testing.T) {
	var s apiServer
	dc, err := DynamicFromKubeconfig(impersonating(writeKubeconfig(t, s.start(t), "")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := callerCtx(t, core.Principal{Subject: "bob"})
	_, _ = dc.Resource(applicationsGVR).Namespace("argocd").Get(ctx, "web", metav1.GetOptions{})
	if s.requests != 1 || s.users[0] != "opsward:bob" || len(s.groups[0]) != 0 {
		t.Fatalf("requests %d, user %q, groups %q", s.requests, s.users, s.groups)
	}
}

// An impersonating profile never falls back to acting as itself.
func TestImpersonatingProfileRefusesARequestWithoutPrincipal(t *testing.T) {
	var s apiServer
	c, err := FromKubeconfig(impersonating(writeKubeconfig(t, s.start(t), "")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AppsV1().Deployments("team-a").Get(t.Context(), "web", metav1.GetOptions{})
	if err == nil || !strings.Contains(err.Error(), "no caller") {
		t.Fatalf("err = %v, want a refusal naming the missing caller", err)
	}
	if s.requests != 0 {
		t.Fatalf("the API server saw %d requests, want none", s.requests)
	}
}

func TestImpersonatingProfileRefusesSystemNames(t *testing.T) {
	var s apiServer
	p := Profile{Kubeconfig: writeKubeconfig(t, s.start(t), ""), Impersonate: &Impersonation{Groups: []string{"system:masters"}}}
	c, err := FromKubeconfig(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []core.Principal{
		{Subject: "system:admin"},
		{Subject: "carol", Groups: []string{"system:masters"}},
	} {
		_, err = c.AppsV1().Deployments("team-a").Get(callerCtx(t, caller), "web", metav1.GetOptions{})
		if err == nil || !strings.Contains(err.Error(), "system:") {
			t.Fatalf("%+v: err = %v, want a refusal of the system: name", caller, err)
		}
	}
	if s.requests != 0 {
		t.Fatalf("the API server saw %d requests, want none", s.requests)
	}
}

func TestImpersonatingProfileRefusesKubeconfigImpersonation(t *testing.T) {
	path := writeKubeconfig(t, "https://127.0.0.1:6443", ", as: someone")
	if _, err := FromKubeconfig(impersonating(path)); err == nil || !strings.Contains(err.Error(), "impersonat") {
		t.Fatalf("FromKubeconfig: err = %v, want a refusal of the kubeconfig's own impersonation", err)
	}
	if _, err := DynamicFromKubeconfig(impersonating(path)); err == nil {
		t.Fatal("DynamicFromKubeconfig must refuse it too")
	}
	if _, err := FromKubeconfig(Profile{Kubeconfig: path}); err != nil {
		t.Fatalf("a profile that does not impersonate keeps the kubeconfig's: %v", err)
	}
}

func TestProfileWithoutImpersonationSendsNoHeaders(t *testing.T) {
	var s apiServer
	c, err := FromKubeconfig(Profile{Kubeconfig: writeKubeconfig(t, s.start(t), "")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.AppsV1().Deployments("team-a").Get(callerCtx(t, core.Principal{Subject: "alice"}), "web", metav1.GetOptions{})
	if s.requests != 1 || s.users[0] != "" {
		t.Fatalf("requests %d, user %q", s.requests, s.users)
	}
}

// A reload that changes who may be impersonated rebuilds the client.
func TestUpdateRebuildsClientWhenImpersonationChanges(t *testing.T) {
	built := 0
	src := NewSource(map[string]string{"prod": "a"},
		map[string]map[string]Profile{"a": {"reader": impersonating("/k")}},
		func(Profile) (kubernetes.Interface, error) { built++; return nil, nil })
	if _, err := src.Workloads("prod", "reader"); err != nil {
		t.Fatal(err)
	}
	src.Update(map[string]string{"prod": "a"}, map[string]map[string]Profile{"a": {"reader": impersonating("/k")}})
	if _, err := src.Workloads("prod", "reader"); err != nil {
		t.Fatal(err)
	}
	if built != 1 {
		t.Fatalf("built %d clients for an unchanged profile, want 1", built)
	}
	next := impersonating("/k")
	next.Impersonate.Groups = []string{"team-a", "team-b"}
	src.Update(map[string]string{"prod": "a"}, map[string]map[string]Profile{"a": {"reader": next}})
	if _, err := src.Workloads("prod", "reader"); err != nil {
		t.Fatal(err)
	}
	if built != 2 {
		t.Fatalf("built %d clients, want a rebuild after the groups changed", built)
	}
}

type recordRT struct{ h http.Header }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.h = req.Header
	return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: req}, nil
}

// Only Opsward decides who a request acts as: impersonation headers set by anything
// before it are dropped, not merged.
func TestImpersonationReplacesEarlierHeaders(t *testing.T) {
	rt := &recordRT{}
	a := &asCaller{rt: rt, imp: Impersonation{UserPrefix: "opsward:", GroupPrefix: "opsward:", Groups: []string{"team-a"}}}
	req, err := http.NewRequestWithContext(callerCtx(t, core.Principal{Subject: "alice", Groups: []string{"team-a"}}), http.MethodGet, "https://127.0.0.1/api", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Impersonate-User", "admin")
	req.Header.Add("Impersonate-Group", "system:masters")
	req.Header.Set("Impersonate-Uid", "0")
	req.Header.Set("Impersonate-Extra-Scopes", "all")
	resp, err := a.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if rt.h.Get("Impersonate-User") != "opsward:alice" || !slices.Equal(rt.h.Values("Impersonate-Group"), []string{"opsward:team-a"}) ||
		rt.h.Get("Impersonate-Uid") != "" || rt.h.Get("Impersonate-Extra-Scopes") != "" {
		t.Fatalf("headers sent: %v", rt.h)
	}
}
