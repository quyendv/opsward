package mcpadapter

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/identity"
)

// tokens is a fake authenticator: token value to subject.
func tokens(m map[string]string) Authenticate {
	return func(_ context.Context, token string) (core.Principal, time.Time, error) {
		switch {
		case token == "boom":
			return core.Principal{}, time.Time{}, errors.New("store unavailable at /var/lib/opsward/opsward.db")
		case m[token] != "":
			return core.Principal{Subject: m[token], Groups: []string{"team-a"}, Credential: "token:" + token}, time.Now().Add(time.Hour), nil
		}
		return core.Principal{}, time.Time{}, errors.Join(identity.ErrUnauthenticated, errors.New("unknown token "+token))
	}
}

func allowAll(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow, Rule: "ok"} }

func httpEngine(t *testing.T, aud *nopAuditor) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.Options{
		Tools:        []engine.Tool{echoTool()},
		Environments: []engine.Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   authorizerFunc(allowAll), Auditor: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func httpClient(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestHTTPCallsRunAsTheTokensPrincipal(t *testing.T) {
	aud := &nopAuditor{}
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, aud), HTTPOptions{Authenticate: tokens(map[string]string{"ta": "alice@example.com", "tb": "bob@example.com"})}))
	defer srv.Close()
	for _, tok := range []string{"ta", "tb"} {
		cs := httpClient(t, srv.URL+"/mcp", tok)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "api"}})
		if err != nil || res.IsError {
			t.Fatalf("call with %s = %+v, %v", tok, res, err)
		}
	}
	if len(aud.principals) != 2 {
		t.Fatalf("audited %d calls", len(aud.principals))
	}
	for i, want := range []string{"alice@example.com", "bob@example.com"} {
		p := aud.principals[i]
		if p.Subject != want || p.Adapter != AdapterHTTP || p.Credential == "" || len(p.Groups) != 1 {
			t.Errorf("principal %d = %+v, want %s over %s", i, p, want, AdapterHTTP)
		}
	}
}

func post(t *testing.T, url string, header map[string]string) (int, string) {
	t.Helper()
	return postBody(t, url, header, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
}

func postBody(t *testing.T, url string, header map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHTTPRejectsWithoutLeakingWhy(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{Authenticate: tokens(map[string]string{"ta": "alice@example.com"})}))
	defer srv.Close()
	for name, tt := range map[string]struct {
		header map[string]string
		status int
		hidden string
	}{
		"no token":      {nil, http.StatusUnauthorized, ""},
		"basic auth":    {map[string]string{"Authorization": "Basic YTpi"}, http.StatusUnauthorized, ""},
		"unknown token": {map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized, "unknown token"},
		"store failure": {map[string]string{"Authorization": "Bearer boom"}, http.StatusServiceUnavailable, "/var/lib"},
		"cross-origin browser": {
			map[string]string{"Authorization": "Bearer ta", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example.com"},
			http.StatusForbidden, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := post(t, srv.URL+"/mcp", tt.header)
			if status != tt.status {
				t.Fatalf("status = %d, want %d; body %q", status, tt.status, body)
			}
			if tt.hidden != "" && strings.Contains(body, tt.hidden) {
				t.Fatalf("body %q tells the client %q", body, tt.hidden)
			}
		})
	}
}

func TestHTTPHealthEndpoints(t *testing.T) {
	var ready error
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: tokens(nil),
		Ready:        func(context.Context) error { return ready },
	}))
	defer srv.Close()
	get := func(path string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if get("/healthz") != 200 || get("/readyz") != 200 {
		t.Fatal("health endpoints not 200")
	}
	ready = errors.New("store closed")
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a failing store = %d", got)
	}
	if get("/healthz") != 200 {
		t.Fatal("/healthz depends on readiness")
	}
	if got := get("/other"); got != http.StatusNotFound {
		t.Fatalf("/other = %d", got)
	}
}

func TestServeHTTPStopsWhenContextEnds(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, ln, httpEngine(t, &nopAuditor{}), HTTPOptions{Authenticate: tokens(map[string]string{"ta": "a"})})
	}()
	cs := httpClient(t, "http://"+ln.Addr().String()+"/mcp", "ta")
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
}

func writeCert(t *testing.T, dir, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// Written to temporary names and renamed, as a mounted secret is updated.
	for name, block := range map[string]*pem.Block{"tls.crt": {Type: "CERTIFICATE", Bytes: der}, "tls.key": {Type: "PRIVATE KEY", Bytes: kb}} {
		tmp := filepath.Join(dir, "."+name)
		if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServeHTTPReloadsTheCertificate(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir, "first")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = ServeHTTP(ctx, ln, httpEngine(t, &nopAuditor{}), HTTPOptions{
			Authenticate: tokens(nil),
			TLS:          &TLSFiles{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")},
		})
	}()
	peer := func() string {
		t.Helper()
		var conn net.Conn
		var err error
		d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // the test reads the served certificate
		for range 50 {
			conn, err = d.DialContext(t.Context(), "tcp", ln.Addr().String())
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return conn.(*tls.Conn).ConnectionState().PeerCertificates[0].Subject.CommonName
	}
	if got := peer(); got != "first" {
		t.Fatalf("certificate = %s", got)
	}
	writeCert(t, dir, "second")
	if got := peer(); got != "second" {
		t.Fatalf("after rotation, certificate = %s", got)
	}
}

func TestHTTPResourceMetadata(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: tokens(nil),
		Resource: &Resource{
			URL: "https://opsward.example.com/mcp", AuthorizationServers: []string{"https://id.example.com"}, Scopes: []string{"opsward"},
		},
	}))
	defer srv.Close()
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			Scopes               []string `json:"scopes_supported"`
			Methods              []string `json:"bearer_methods_supported"`
		}
		err = json.NewDecoder(resp.Body).Decode(&m)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || m.Resource != "https://opsward.example.com/mcp" ||
			len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != "https://id.example.com" || len(m.Scopes) != 1 || len(m.Methods) != 1 || m.Methods[0] != "header" {
			t.Fatalf("%s: %d %+v %v", path, resp.StatusCode, m, err)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	want := `Bearer resource_metadata="https://opsward.example.com/.well-known/oauth-protected-resource/mcp", scope="opsward"`
	if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || got != want {
		t.Fatalf("401 challenge = %q, want %q", got, want)
	}
}

func TestHTTPNoResourceMetadataWithoutOIDC(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{Authenticate: tokens(nil)}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/.well-known/oauth-protected-resource", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata without OIDC = %d", resp.StatusCode)
	}
}

func TestMetadataURL(t *testing.T) {
	for resource, want := range map[string]string{
		"https://opsward.example.com/mcp":       "https://opsward.example.com/.well-known/oauth-protected-resource/mcp",
		"https://example.com/tools/opsward/mcp": "https://example.com/.well-known/oauth-protected-resource/tools/opsward/mcp",
		"http://127.0.0.1:8080/mcp":             "http://127.0.0.1:8080/.well-known/oauth-protected-resource/mcp",
	} {
		if got := metadataURL(resource); got != want {
			t.Errorf("metadataURL(%s) = %s, want %s", resource, got, want)
		}
	}
}

// Behind a path, metadata is where RFC 9728 puts it for that resource.
func TestHTTPResourceMetadataUnderAPath(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: tokens(nil),
		Resource:     &Resource{URL: "https://example.com/tools/opsward/mcp", AuthorizationServers: []string{"https://id.example.com"}},
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/.well-known/oauth-protected-resource/tools/opsward/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// Other endpoints, such as the alert webhook, authenticate like /mcp and get the
// token's principal.
func TestHTTPRoutesAuthenticate(t *testing.T) {
	var got core.Principal
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: tokens(map[string]string{"ta": "alertmanager"}),
		Routes: map[string]PrincipalHandler{"/alerts/alertmanager": func(w http.ResponseWriter, _ *http.Request, p core.Principal) {
			got = p
			w.WriteHeader(http.StatusAccepted)
		}},
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/alerts/alertmanager", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || got.Subject != "" {
		t.Fatalf("without a token: %d, %+v", resp.StatusCode, got)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/alerts/alertmanager", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer ta")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || got.Subject != "alertmanager" {
		t.Fatalf("with a token: %d, %+v", resp.StatusCode, got)
	}
}

// Handlers that authenticate themselves, such as the console, are served as they are.
func TestHTTPHandlersAreServedAsTheyAre(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: tokens(map[string]string{}),
		Handlers: map[string]http.Handler{"/console/": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})},
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/console/login", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
