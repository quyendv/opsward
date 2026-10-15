package redact

import (
	"strings"
	"testing"
)

// Fixtures are assembled at run time so that secret scanners do not flag this file.
func join(parts ...string) string { return strings.Join(parts, "") }

func TestStringRedacts(t *testing.T) {
	tests := []struct {
		name, in, keep string
		secret         string
	}{
		{"bearer token", "Authorization: Bearer " + join("abc", "DEF123", "xyz.987"), "Authorization: Bearer ", join("abc", "DEF123")},
		{"aws access key", "key " + join("AKIA", "ABCDEFGHIJKLMNOP"), "key ", join("AKIA", "ABCDEFGHIJKLMNOP")},
		{"github token", join("gh", "p_", strings.Repeat("a1B2", 9)), "", strings.Repeat("a1B2", 9)},
		{"jwt", join("ey", "JhbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTYifQ", ".", "c2lnbmF0dXJlLXZhbHVl"), "", "c2lnbmF0dXJlLXZhbHVl"},
		{"url credentials", join("postgres://app:", "s3cr3tpass", "@db.example.com:5432/app"), "postgres://app:", "s3cr3tpass"},
		{"key value", join("DB_PASSWORD=", "hunter2hunter2"), "DB_PASSWORD=", "hunter2hunter2"},
		{"json-ish key value", join(`"api_key": "`, "k-123456789", `"`), `"api_key": "`, "k-123456789"},
		{"private key", join("-----BEGIN RSA ", "PRIVATE KEY-----\nMIIEow\n-----END RSA ", "PRIVATE KEY-----"), "", "MIIEow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.in)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("String(%q) = %q, secret still present", tt.in, got)
			}
			if !strings.Contains(got, Placeholder) || !strings.HasPrefix(got, tt.keep) {
				t.Fatalf("String(%q) = %q, want prefix %q and a placeholder", tt.in, got, tt.keep)
			}
		})
	}
}

func TestStringLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"Back-off restarting failed container api in pod api-7d9f",
		"readiness probe failed: HTTP probe failed with statuscode: 503",
		"https://grafana.example.com/d/abc?orgId=1",
		"token bucket rate limiter configured",
		"secretName: tls-cert",
	} {
		if got := String(s); got != s {
			t.Errorf("String(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestValueWalksNestedJSON(t *testing.T) {
	in := map[string]any{
		"name":     "api",
		"password": "plain",
		"env":      []any{map[string]any{"name": "URL", "value": join("redis://u:", "pw123456", "@cache:6379")}},
		"replicas": float64(3),
	}
	out := Value(in).(map[string]any)
	if out["password"] != Placeholder {
		t.Errorf("password = %v", out["password"])
	}
	env := out["env"].([]any)[0].(map[string]any)
	if strings.Contains(env["value"].(string), "pw123456") {
		t.Errorf("nested value not redacted: %v", env["value"])
	}
	if out["name"] != "api" || out["replicas"] != float64(3) {
		t.Errorf("ordinary fields changed: %+v", out)
	}
	if in["password"] != "plain" {
		t.Error("Value modified its input")
	}
}
