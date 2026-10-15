package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store/sqlite"
)

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opsward.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateExampleConfig(t *testing.T) {
	code, out, errOut := runCLI("validate", "--config", "../../examples/opsward.yaml")
	if code != 0 || !strings.Contains(out, "configuration is valid") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestValidateReportsErrors(t *testing.T) {
	cfg := writeConfig(t, "identity: {principal: {subject: me}}\nstorage: {path: x.db}\npolicy:\n  bindings: [{name: b, role: missing}]\n")
	code, _, errOut := runCLI("validate", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, `unknown role "missing"`) {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestValidatePrintsWarnings(t *testing.T) {
	cfg := writeConfig(t, `identity: {principal: {subject: me}}
storage: {path: x.db}
policy:
  roles: {viewer: {risk: [read]}}
  bindings: [{name: b, role: viewer, when: 'now.getHours() < 18'}]
`)
	code, out, _ := runCLI("validate", "--config", cfg)
	if code != 0 || !strings.Contains(out, "warning:") || !strings.Contains(out, "time zone") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestServeRefusesStdoutAuditStream(t *testing.T) {
	cfg := writeConfig(t, "identity: {principal: {subject: me}}\nstorage: {path: x.db}\naudit: {stream: {type: stdout}}\n")
	code, _, errOut := runCLI("serve", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "would corrupt MCP over stdio") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, errOut := runCLI(); code != 2 || !strings.Contains(errOut, "usage:") {
		t.Fatalf("no args: code %d, stderr %q", code, errOut)
	}
	if code, _, _ := runCLI("validate"); code != 1 {
		t.Fatalf("validate without --config: code %d", code)
	}
	if code, out, _ := runCLI("version"); code != 0 || !strings.HasPrefix(out, "opsward ") {
		t.Fatalf("version: code %d, stdout %q", code, out)
	}
}

func storageConfig(t *testing.T, db string) string {
	t.Helper()
	return writeConfig(t, "identity: {principal: {subject: me}}\nstorage: {path: "+db+"}\n")
}

func TestAuditVerifyRefusesAMissingDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing.db")
	code, _, errOut := runCLI("audit", "verify", "--config", storageConfig(t, db))
	if code != 1 || !strings.Contains(errOut, "no database") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("audit verify created the database it was asked to verify")
	}
}

func TestAuditVerifyRefusesAnEmptyStore(t *testing.T) {
	db := filepath.Join(t.TempDir(), "empty.db")
	s, err := sqlite.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	code, _, errOut := runCLI("audit", "verify", "--config", storageConfig(t, db))
	if code != 1 || !strings.Contains(errOut, "no audit records") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestAuditVerifyChecksEveryWriter(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "a.db")
	s, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"host-a", "host-b"} {
		w, err := audit.NewWriter(ctx, audit.Options{WriterID: id, Store: s, Stream: io.Discard, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(ctx, audit.Record{Phase: audit.PhaseOutcome, Principal: core.Principal{Subject: "me"}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	code, out, errOut := runCLI("audit", "verify", "--config", storageConfig(t, db))
	if code != 0 || !strings.Contains(out, `"host-a": 1 records verified`) || !strings.Contains(out, `"host-b": 1 records verified`) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
