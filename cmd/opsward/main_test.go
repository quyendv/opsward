package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
