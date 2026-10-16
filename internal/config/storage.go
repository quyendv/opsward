package config

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Storage types.
const (
	StorageSQLite   = "sqlite"
	StoragePostgres = "postgres"
)

// SecretRef references a secret by file or environment variable, never inline.
type SecretRef struct {
	File string `yaml:"file"`
	Env  string `yaml:"env"`
}

// UnmarshalYAML rejects an inline value without repeating it: it is likely a secret.
func (r *SecretRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a secret (storage.dsn, a forge token, an alert notifier) must be { file: PATH } or { env: NAME }; an inline value is not accepted", n.Line)
	}
	type plain SecretRef
	return n.Decode((*plain)(r))
}

// Resolve reads the secret. field names it in errors; the value never appears in one.
func (r SecretRef) Resolve(field string) (string, error) {
	if r.File != "" {
		b, err := os.ReadFile(r.File)
		if err != nil {
			return "", fmt.Errorf("%s.file: %w", field, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	v := strings.TrimSpace(os.Getenv(r.Env))
	if v == "" {
		return "", fmt.Errorf("%s.env: %s is empty", field, r.Env)
	}
	return v, nil
}

func (c *Config) validateStorage(fail func(string, ...any)) {
	s := c.Storage
	switch s.Type {
	case StorageSQLite:
		if s.Path == "" {
			fail("storage.path is required")
		}
		if s.DSN != nil {
			fail("storage.dsn is used only with postgres")
		}
	case StoragePostgres:
		if s.Path != "" {
			fail("storage.path is used only with sqlite")
		}
		switch {
		case s.DSN == nil || (s.DSN.File == "" && s.DSN.Env == ""):
			fail("storage.dsn is required for postgres: { file: PATH } or { env: NAME }")
		case s.DSN.File != "" && s.DSN.Env != "":
			fail("storage.dsn: set one of file and env")
		case strings.HasPrefix(s.DSN.File, "~"):
			fail("storage.dsn.file: ~ is not expanded, use an absolute path")
		}
	default:
		fail("storage.type: unknown type %q; use %s or %s", s.Type, StorageSQLite, StoragePostgres)
	}
}
