package httpapi

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGetSendsAuthAndTenantAndReadsTokenEachTime(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Scope-OrgID")+"|"+r.URL.Path+"?"+r.URL.RawQuery)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	token := writeFile(t, "token-1\n")
	c, err := New(Options{URL: srv.URL + "/prefix", Tenant: "team-a", BearerTokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := c.Get(context.Background(), "/api/v1/query", url.Values{"query": {"up"}}); err != nil || string(b) != "ok" {
		t.Fatalf("Get = %q, %v", b, err)
	}
	if err := os.WriteFile(token, []byte("token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if seen[0] != "Bearer token-1|team-a|/prefix/api/v1/query?query=up" || !strings.HasPrefix(seen[1], "Bearer token-2|") {
		t.Fatalf("requests = %q", seen)
	}
}

func TestGetBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "opsward" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c, err := New(Options{URL: srv.URL, BasicUsername: "opsward", BasicPasswordFile: writeFile(t, "pw\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "/", nil); err != nil {
		t.Fatal(err)
	}
}

func TestGetErrorsAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","error":"parse error"}`))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 64)))
		}
	}))
	defer srv.Close()
	c, err := New(Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "/fail", nil); err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("err = %v", err)
	}
	c.maxBody = 32
	if _, err := c.Get(context.Background(), "/big", nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized: err = %v", err)
	}
}

func TestGetTrustsTheConfiguredCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("tls")) }))
	defer srv.Close()
	ca := writeFile(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	c, err := New(Options{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := c.Get(context.Background(), "/", nil); err != nil || string(b) != "tls" {
		t.Fatalf("Get = %q, %v", b, err)
	}
	plain, _ := New(Options{URL: srv.URL})
	if _, err := plain.Get(context.Background(), "/", nil); err == nil {
		t.Fatal("an unknown CA must be refused without caFile")
	}
}
