package mcpadapter_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	mcpadapter "github.com/quyendv/opsward/internal/adapter/mcp"
	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/identity/oidctest"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

const e2eRemoteConfig = `
identity:
  mode: remote
  groups:
    team-a: [alice@example.com]
server:
  http: { address: 127.0.0.1:0 }
storage:
  path: %DB%
audit:
  writer: test
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader: { kubeconfig: /unused }
environments:
  - name: staging
    cluster: cluster-a
policy:
  roles:
    viewer: { risk: [read] }
  bindings:
    - name: team-a-view
      subjects: { groups: [team-a] }
      role: viewer
`

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// TestEndToEndRemote: two people with API tokens over HTTP. Only alice is in team-a,
// through an internal group; bob's token works but grants him nothing; a revoked token
// stops working on the next request.
func TestEndToEndRemote(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Parse([]byte(strings.Replace(e2eRemoteConfig, "%DB%", filepath.Join(t.TempDir(), "o.db"), 1)))
	if err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}})
	a, err := app.Open(ctx, cfg, app.Options{
		Stderr:        &bytes.Buffer{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	srv := httptest.NewServer(mcpadapter.NewHTTPHandler(a.Engine, mcpadapter.HTTPOptions{Authenticate: a.Identity.Authenticate, Ready: a.Ready}))
	defer srv.Close()

	issue := func(subject string) (string, string) {
		tok, rec, err := a.Identity.CreateToken(ctx, identity.TokenRequest{Subject: subject, TTL: time.Hour, MaxTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return tok, rec.ID
	}
	call := func(token string) (*mcp.CallToolResult, error) {
		tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, tr, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = cs.Close() }()
		return cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_workload_status", Arguments: map[string]any{
			"environment": "staging", "namespace": "shop", "kind": "Deployment", "name": "api",
		}})
	}
	alice, aliceID := issue("alice@example.com")
	bob, _ := issue("bob@example.com")

	if res, err := call(alice); err != nil || res.IsError {
		t.Fatalf("alice: %+v, %v", res, err)
	}
	if res, err := call(bob); err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "denied by policy") {
		t.Fatalf("bob: %+v, %v", res, err)
	}
	if _, err := a.Store.RevokeAPIToken(ctx, aliceID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := call(alice); err == nil {
		t.Fatal("a revoked token still connects")
	}

	records, err := audit.Load(ctx, a.Store, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("audited %d calls, want 2: a rejected token is not a call", len(records))
	}
	if p := records[0].Principal; p.Subject != "alice@example.com" || p.Adapter != mcpadapter.AdapterHTTP || p.Credential != "token:"+aliceID {
		t.Fatalf("alice's record principal = %+v", p)
	}
	if records[1].Principal.Subject != "bob@example.com" || records[1].Outcome.Status != audit.StatusDenied {
		t.Fatalf("bob's record = %+v", records[1])
	}
}

// TestEndToEndOIDC: an access token from the issuer, its groups claim prefixed, reaches
// a tool; the principal is audited with credential "oidc".
func TestEndToEndOIDC(t *testing.T) {
	ctx := context.Background()
	is := oidctest.New(t)
	doc := strings.Replace(e2eRemoteConfig, "%DB%", filepath.Join(t.TempDir(), "o.db"), 1)
	doc = strings.Replace(doc, "  mode: remote\n", "  mode: remote\n  oidc: { issuer: "+is.URL+", groupsPrefix: \"oidc:\" }\n", 1)
	doc = strings.Replace(doc, "  http: { address: 127.0.0.1:0 }\n", "  http: { address: 127.0.0.1:0, publicURL: https://opsward.example.com }\n", 1)
	doc = strings.Replace(doc, "      subjects: { groups: [team-a] }\n", "      subjects: { groups: [\"oidc:team-a\"] }\n", 1)
	doc = strings.Replace(doc, "  groups:\n    team-a: [alice@example.com]\n", "", 1)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}})
	a, err := app.Open(ctx, cfg, app.Options{
		Stderr:        &bytes.Buffer{},
		ClientFactory: func(kubernetes.Profile) (k8s.Interface, error) { return cluster, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Identity.Discover(dctx)
	srv := httptest.NewServer(mcpadapter.NewHTTPHandler(a.Engine, mcpadapter.HTTPOptions{Authenticate: a.Identity.Authenticate}))
	defer srv.Close()

	token := is.Sign(is.Claims("https://opsward.example.com/mcp", nil))
	var res *mcp.CallToolResult
	for range 100 { // discovery runs in the background
		tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}}
		cs, cerr := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, tr, nil)
		if cerr != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_workload_status", Arguments: map[string]any{
			"environment": "staging", "namespace": "shop", "kind": "Deployment", "name": "api",
		}})
		_ = cs.Close()
		break
	}
	if res == nil || err != nil || res.IsError {
		t.Fatalf("call = %+v, %v", res, err)
	}
	records, err := audit.Load(ctx, a.Store, "test")
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	if p := records[0].Principal; p.Subject != "alice@example.com" || p.Credential != "oidc" || !slices.Contains(p.Groups, "oidc:team-a") {
		t.Fatalf("principal = %+v", p)
	}
}
