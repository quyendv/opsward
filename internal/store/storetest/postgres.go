package storetest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Environment variables for tests that need PostgreSQL. CI sets both, so that a
// missing database fails the run instead of skipping the tests.
const (
	PostgresDSNEnv     = "OPSWARD_TEST_POSTGRES_DSN"
	RequirePostgresEnv = "OPSWARD_REQUIRE_POSTGRES"
)

// PostgresSchema creates an empty schema, dropped when the test ends, and returns a DSN
// whose search_path is that schema. It skips the test without PostgresDSNEnv.
func PostgresSchema(t *testing.T) string {
	t.Helper()
	base := os.Getenv(PostgresDSNEnv)
	if base == "" {
		if os.Getenv(RequirePostgresEnv) != "" {
			t.Fatalf("%s is set but %s is not", RequirePostgresEnv, PostgresDSNEnv)
		}
		t.Skip(PostgresDSNEnv + " is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "t_" + strings.ToLower(rand.Text())[:16]
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(context.Background())
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL: %v", PostgresDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
