package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/identity"
)

const remoteDoc = `
identity:
  mode: remote
  apiTokens: { maxTTL: 30d }
  groups:
    platform: [alice@example.com]
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
`

func remoteFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opsward.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(remoteDoc, "%DB%", filepath.Join(dir, "o.db"), 1)), 0o600); err != nil {
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
	return a, path
}

func TestRemoteAppAuthenticatesWithInternalGroups(t *testing.T) {
	a, path := remoteFixture(t)
	ctx := context.Background()
	tok, _, err := a.Identity.CreateToken(ctx, identity.TokenRequest{Subject: "alice@example.com", TTL: time.Hour, MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := a.Identity.Authenticate(ctx, tok)
	if err != nil || !slices.Equal(p.Groups, []string{"platform"}) {
		t.Fatalf("principal = %+v, %v", p, err)
	}

	b, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own file in t.TempDir
	next := strings.Replace(string(b), "platform: [alice@example.com]", "sre: [alice@example.com]", 1)
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil { //nolint:gosec // G703: as above
		t.Fatal(err)
	}
	if err := a.ReloadFile(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	p, _, _ = a.Identity.Authenticate(ctx, tok)
	if !slices.Equal(p.Groups, []string{"sre"}) {
		t.Fatalf("after reload, groups = %v", p.Groups)
	}
	if err := a.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}

func TestAdminTokenLifecycleIsAudited(t *testing.T) {
	a, path := remoteFixture(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Close() // the CLI runs while the server may be running; here it runs alone
	ctx := context.Background()
	adm, err := OpenAdmin(ctx, cfg, Options{Stderr: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adm.Close() }()

	if _, _, err := adm.CreateToken(ctx, "root", identity.TokenRequest{Subject: "ci@example.com", TTL: 31 * 24 * time.Hour}); err == nil ||
		!strings.Contains(err.Error(), "maxTTL") {
		t.Fatalf("a token over maxTTL: %v", err)
	}
	tok, rec, err := adm.CreateToken(ctx, "root", identity.TokenRequest{Subject: "ci@example.com", Groups: []string{"deployers"}, TTL: time.Hour, Description: "pipeline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := adm.RevokeToken(ctx, "root", rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := adm.RevokeToken(ctx, "root", "nonexistent0"); err == nil {
		t.Fatal("revoking an unknown token succeeded")
	}
	if _, _, err := adm.Identity.Authenticate(ctx, tok); err == nil {
		t.Fatal("a revoked token authenticates")
	}

	records, err := audit.Load(ctx, adm.Store, "test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records {
		got = append(got, string(r.Phase)+" "+r.Tool+" "+status(r))
		if r.Principal.Subject != "root" || r.Principal.Adapter != "cli" || r.Risk != "destructive" {
			t.Errorf("record %+v", r)
		}
		if strings.Contains(string(mustJSON(t, r)), strings.TrimPrefix(tok, "opw_"+rec.ID+"_")) {
			t.Fatal("the token secret reached the audit record")
		}
	}
	want := []string{
		"intent create_api_token ", "outcome create_api_token ok",
		"intent revoke_api_token ", "outcome revoke_api_token ok",
		"intent revoke_api_token ", "outcome revoke_api_token error",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := audit.Verify(records); err != nil {
		t.Fatal(err)
	}
	if records[0].Args["id"] != rec.ID || records[0].Args["subject"] != "ci@example.com" {
		t.Fatalf("intent args = %v", records[0].Args)
	}
}

func status(r audit.Record) string {
	if r.Outcome == nil {
		return ""
	}
	return r.Outcome.Status
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
