package app

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := adm.RevokeToken(ctx, "root", "aaaaaaaaaaaa"); err == nil {
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

// Revoking with the whole token, the obvious mistake after a leak, must revoke it and
// keep the secret out of the audit chain.
func TestRevokeWithTheWholeTokenNeverRecordsTheSecret(t *testing.T) {
	_, path := remoteFixture(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var stream strings.Builder
	adm, err := OpenAdmin(ctx, cfg, Options{Stderr: &stream})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adm.Close() }()
	tok, rec, err := adm.CreateToken(ctx, "root", identity.TokenRequest{Subject: "ci@example.com", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(tok, "opw_"+rec.ID+"_")
	if err := adm.RevokeToken(ctx, "root", tok); err != nil {
		t.Fatalf("revoke by token: %v", err)
	}
	if _, _, err := adm.Identity.Authenticate(ctx, tok); err == nil {
		t.Fatal("the token still authenticates")
	}
	for _, bad := range []string{"opw_" + rec.ID + "_" + secret[:10] + "XYZ", "not-an-id", ""} {
		err := adm.RevokeToken(ctx, "root", bad)
		if err == nil || strings.Contains(err.Error(), secret[:10]) {
			t.Fatalf("RevokeToken(%q) = %v", bad, err)
		}
	}
	records, _ := audit.Load(ctx, adm.Store, "test")
	for _, r := range records {
		if strings.Contains(string(mustJSON(t, r)), secret[:20]) {
			t.Fatalf("record %s %s holds the secret", r.Phase, r.Tool)
		}
	}
	if strings.Contains(stream.String(), secret[:20]) {
		t.Fatal("the audit stream holds the secret")
	}
}

// The CLI prints the token on stdout; audit records there would corrupt it.
func TestAdminNeverStreamsToStdout(t *testing.T) {
	_, path := remoteFixture(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Audit.Stream.Type = "stdout"
	var stdout, stderr strings.Builder
	adm, err := OpenAdmin(context.Background(), cfg, Options{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adm.Close() }()
	if _, _, err := adm.CreateToken(context.Background(), "root", identity.TokenRequest{Subject: "a", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "create_api_token") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// failAfter accepts n writes, then fails every write.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("stream closed")
	}
	f.n--
	return len(p), nil
}

// An interrupt after the intent record must not lose the outcome record.
func TestAdminOutcomeSurvivesCancellation(t *testing.T) {
	_, path := remoteFixture(t)
	cfg, _ := config.Load(path)
	adm, err := OpenAdmin(context.Background(), cfg, Options{Stderr: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adm.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	_ = adm.audited(ctx, "root", "revoke_api_token", map[string]any{"id": "aaaaaaaaaaaa"}, func() error { cancel(); return ctx.Err() })
	records, _ := audit.Load(context.Background(), adm.Store, "test")
	if len(records) != 2 || records[1].Phase != audit.PhaseOutcome {
		t.Fatalf("records = %d, want intent and outcome", len(records))
	}
}

// When the outcome of a create cannot be audited, the operator must learn which stored
// token to revoke: its secret is gone.
func TestAdminCreateWhoseOutcomeFailsNamesTheToken(t *testing.T) {
	_, path := remoteFixture(t)
	cfg, _ := config.Load(path)
	adm, err := OpenAdmin(context.Background(), cfg, Options{Stderr: &failAfter{n: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adm.Close() }()
	_, _, err = adm.CreateToken(context.Background(), "root", identity.TokenRequest{Subject: "a", TTL: time.Hour})
	list, _ := adm.Store.ListAPITokens(context.Background())
	if err == nil || len(list) != 1 || !strings.Contains(err.Error(), list[0].ID) || !strings.Contains(err.Error(), "revoke") {
		t.Fatalf("err = %v, tokens %d", err, len(list))
	}
}
