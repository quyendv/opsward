// Package redact removes credential-shaped content from tool output before it leaves
// the engine. It is a safety net, not the boundary: profiles cannot read Secrets and
// tools do not ask for them.
package redact

import (
	"regexp"
	"slices"
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
		if lines, ok := stringSlice(out); ok {
			for i, l := range keyLines(lines) {
				out[i] = l
			}
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

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// A base64 line, optionally after a log timestamp ("2026-01-02T03:04:05.123Z ").
	base64Line = regexp.MustCompile(`^(?:\S+Z\s+)?[A-Za-z0-9+/=]{16,}\s*$`)
)

// keyLines redacts private keys that span several entries of a line array, such as log
// output: everything from a BEGIN line to its END line, or to the end when the END line
// is missing, and the base64 lines before an END line whose BEGIN was cut off.
func keyLines(lines []string) []string {
	out := slices.Clone(lines)
	inside := false
	for i, l := range lines {
		switch {
		case inside:
			out[i] = Placeholder
			inside = !pemEnd.MatchString(l)
		case pemBegin.MatchString(l):
			out[i] = Placeholder
			inside = !pemEnd.MatchString(l)
		case pemEnd.MatchString(l):
			out[i] = Placeholder
			for j := i - 1; j >= 0 && base64Line.MatchString(lines[j]); j-- {
				out[j] = Placeholder
			}
		}
	}
	return out
}

func stringSlice(v []any) ([]string, bool) {
	out := make([]string, len(v))
	for i, e := range v {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, len(out) > 1
}
