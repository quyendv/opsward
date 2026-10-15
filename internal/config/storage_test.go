package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withStorage(storage string) string {
	return strings.Replace(minimal, "storage:\n  path: /var/lib/opsward/opsward.db\n", "storage:\n"+storage, 1)
}

func TestPostgresStorage(t *testing.T) {
	c, err := Parse([]byte(withStorage("  type: postgres\n  dsn: { file: /etc/opsward/postgres-dsn }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Type != "postgres" || c.Storage.DSN.File != "/etc/opsward/postgres-dsn" {
		t.Fatalf("storage = %+v", c.Storage)
	}
}

func TestStorageRejects(t *testing.T) {
	for name, tt := range map[string]struct{ storage, want string }{
		"postgres without dsn":   {"  type: postgres\n", "storage.dsn is required for postgres"},
		"postgres with path":     {"  type: postgres\n  path: /x.db\n  dsn: { env: PG }\n", "storage.path is used only with sqlite"},
		"dsn file and env":       {"  type: postgres\n  dsn: { file: /x, env: PG }\n", "storage.dsn: set one of file and env"},
		"sqlite with dsn":        {"  path: /x.db\n  dsn: { env: PG }\n", "storage.dsn is used only with postgres"},
		"unknown type":           {"  type: mysql\n", `storage.type: unknown type "mysql"`},
		"inline dsn not allowed": {"  type: postgres\n  dsn: postgres://u:p@h/db\n", "storage.dsn"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withStorage(tt.storage)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "u:p@h") {
				t.Fatalf("error echoes the inline DSN: %v", err)
			}
		})
	}
}

func TestResolveDSN(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "dsn")
	if err := os.WriteFile(f, []byte("postgres://u@h/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPSWARD_TEST_DSN", "postgres://e@h/db")
	for name, tt := range map[string]struct {
		ref  SecretRef
		want string
		err  string
	}{
		"file":         {SecretRef{File: f}, "postgres://u@h/db", ""},
		"env":          {SecretRef{Env: "OPSWARD_TEST_DSN"}, "postgres://e@h/db", ""},
		"missing file": {SecretRef{File: filepath.Join(dir, "nope")}, "", "storage.dsn.file"},
		"empty env":    {SecretRef{Env: "OPSWARD_TEST_UNSET"}, "", "OPSWARD_TEST_UNSET is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tt.ref.Resolve("storage.dsn")
			if got != tt.want || (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("Resolve = %q, %v; want %q, %q", got, err, tt.want, tt.err)
			}
		})
	}
}
