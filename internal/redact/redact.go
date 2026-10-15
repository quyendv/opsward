// Package redact removes credential-shaped content from tool output before it leaves
// the engine. It is a safety net, not the boundary: profiles cannot read Secrets and
// tools do not ask for them.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces every redacted value.
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// Each rule keeps enough context (the key name, the URL user) for the output to stay
// readable, and replaces only the secret part.
var rules = []rule{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), Placeholder},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]+=*`), "${1}" + Placeholder},
	{regexp.MustCompile(`(?i)\b(authorization:\s*basic\s+)[A-Za-z0-9+/]+=*`), "${1}" + Placeholder},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Placeholder},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), Placeholder},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9\-_]{20,}\b`), Placeholder},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), Placeholder},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Placeholder},
	{regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^:/\s@]+:)[^@\s/]+@`), "${1}" + Placeholder + "@"},
	{regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)["']?(?::|\s*=)\s*["']?)[^\s"',;&]+`), "${1}" + Placeholder},
}

// String returns s with credential-shaped content replaced.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Value returns a copy of v, a decoded JSON value, with every string redacted. Map keys
// are kept: they name fields, and a key such as "password" is what makes the
// key/value rule work on the serialized form.
func Value(v any) any {
	switch t := v.(type) {
	case string:
		return String(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = valueFor(k, val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Value(val)
		}
		return out
	default:
		return v
	}
}

// sensitiveKey matches a map key, lower-cased with separators removed, that ends in a
// credential word: DB_PASSWORD, dbPassword and x-api-key match; secretName does not.
var sensitiveKey = regexp.MustCompile(`(password|passwd|pwd|secret|token|apikey|accesskey|privatekey)$`)

var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")

func valueFor(key string, v any) any {
	norm := keySeparators.Replace(strings.ToLower(key))
	if s, ok := v.(string); ok && sensitiveKey.MatchString(norm) && s != "" {
		return Placeholder
	}
	return Value(v)
}
