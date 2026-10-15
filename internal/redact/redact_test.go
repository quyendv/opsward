package redact

import (
	"fmt"
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
		{"opsward api token", "leaked " + join("opw_", "abcdabcdabcd", "_", strings.Repeat("k2m7", 13)), "leaked ", strings.Repeat("k2m7", 13)},
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

func TestStringRedactsBasicAuth(t *testing.T) {
	in := "Authorization: Basic " + join("dXNlcjpw", "YXNzd29yZA==")
	if got := String(in); strings.Contains(got, "YXNzd29yZA") || !strings.HasPrefix(got, "Authorization: Basic ") {
		t.Fatalf("String(%q) = %q", in, got)
	}
	if prose := "basic auth is enabled"; String(prose) != prose {
		t.Fatalf("String(%q) = %q, want unchanged", prose, String(prose))
	}
}

// Kubelet event messages name Secrets and keys; they carry no secret values and must
// stay readable, because they are what diagnosis needs.
func TestStringLeavesKubeletMessagesAlone(t *testing.T) {
	for _, s := range []string{
		`MountVolume.SetUp failed for volume "app-token" : secret "app-token" not found`,
		`Error: secret "db-secret" : key password not found`,
		`couldn't find key password in Secret shop/db-secret`,
	} {
		if got := String(s); got != s {
			t.Errorf("String(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestValueMatchesKeyNamesLoosely(t *testing.T) {
	out := Value(map[string]any{
		"DB_PASSWORD": "a", "dbPassword": "b", "clientSecret": "c", "x-api-key": "d",
		"secretName": "keep", "tokenTTL": "keep",
	}).(map[string]any)
	for _, k := range []string{"DB_PASSWORD", "dbPassword", "clientSecret", "x-api-key"} {
		if out[k] != Placeholder {
			t.Errorf("%s = %v, want redacted", k, out[k])
		}
	}
	for _, k := range []string{"secretName", "tokenTTL"} {
		if out[k] != "keep" {
			t.Errorf("%s = %v, want kept", k, out[k])
		}
	}
}

// Log tools return output as an array of lines. A private key spans several lines, so
// it must be redacted across them, including when the tail window cut off its BEGIN or
// END line.
func TestValueRedactsPrivateKeysAcrossLines(t *testing.T) {
	begin, end := join("-----BEGIN RSA ", "PRIVATE KEY-----"), join("-----END RSA ", "PRIVATE KEY-----")
	body1, body2 := join("MIIEow", "IBAAKCAQEA0Z3VS5JJcds3xfn"), join("secretbody", "Q2xvdWQgS2V5")
	for name, lines := range map[string][]any{
		"whole key":     {"starting", begin, body1, body2, end, "listening on :8080"},
		"no end line":   {"starting", begin, body1, body2},
		"no begin line": {body1, body2, end, "listening on :8080"},
	} {
		t.Run(name, func(t *testing.T) {
			out := Value(map[string]any{"lines": lines}).(map[string]any)["lines"].([]any)
			joined := fmt.Sprint(out...)
			if strings.Contains(joined, "MIIEow") || strings.Contains(joined, "secretbody") {
				t.Fatalf("key material survived: %q", out)
			}
			if len(out) != len(lines) {
				t.Fatalf("line count changed: %d -> %d", len(lines), len(out))
			}
			for i, l := range lines {
				if l == "starting" || l == "listening on :8080" {
					if out[i] != l {
						t.Fatalf("ordinary line %d changed: %q", i, out[i])
					}
				}
			}
		})
	}
}

func TestValueLeavesOrdinaryLineArraysAlone(t *testing.T) {
	lines := []any{"GET /healthz 200", "QmFzZTY0IGlzIG5vdCBhIGtleQ==", "done"}
	out := Value(lines).([]any)
	for i := range lines {
		if out[i] != lines[i] {
			t.Fatalf("line %d changed: %q", i, out[i])
		}
	}
}
