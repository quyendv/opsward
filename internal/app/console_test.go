package app

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/identity"
)

const consolePolicy = `
console:
  apiTokens: true
policy:
  roles:
    viewer: { risk: [read] }
    auditor: { tools: [search_audit] }
  bindings:
    - name: platform-audits
      subjects: { groups: [platform] }
      role: auditor
`

// The console reads through the engine with the session's principal, and shows what
// the current policy says about them.
func TestConsoleThroughTheApp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opsward.yaml")
	doc := strings.Replace(remoteDoc, "%DB%", filepath.Join(dir, "o.db"), 1) + consolePolicy
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), cfg, Options{Stderr: &strings.Builder{}, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	h, err := a.Console()
	if err != nil || h == nil {
		t.Fatalf("Console: %v, %v", h, err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)

	tok, _, err := a.Identity.CreateToken(context.Background(), identity.TokenRequest{Subject: "alice@example.com", TTL: time.Hour, MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/console/login/token", strings.NewReader(url.Values{"token": {tok}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	get := func(path string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	if body := get("/console/you"); !strings.Contains(body, "platform-audits") {
		t.Fatalf("you:\n%s", body)
	}
	_ = get("/console/approvals")
	// The audit shows the console's own calls, made as the person through the console.
	if body := get("/console/audit"); !strings.Contains(body, "list_approvals") || !strings.Contains(body, "console") {
		t.Fatalf("audit:\n%s", body)
	}
}

func TestNoConsoleWithoutTheSection(t *testing.T) {
	a, _ := remoteFixture(t)
	if h, err := a.Console(); h != nil || err != nil {
		t.Fatalf("Console = %v, %v", h, err)
	}
}
