package redact

import (
	"regexp"
	"strings"
)

var (
	secretKind  = regexp.MustCompile(`^kind:\s*["']?Secret["']?\s*(#.*)?$`)
	secretField = regexp.MustCompile(`^(data|stringData):\s*(.*)$`)
	entry       = regexp.MustCompile(`^(\s+)([^\s:#][^:]*):`)
)

// Manifests blanks the values of data and stringData in the YAML documents of s that
// are Kubernetes Secrets, keeping their keys. Credential-shaped rules miss these
// values: they are base64 or arbitrary text under ordinary key names.
func Manifests(s string) string {
	if !strings.Contains(s, "Secret") {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	var out []string
	start := 0
	flush := func(end int) {
		doc := lines[start:end]
		if isSecret(doc) {
			doc = blankData(doc)
		}
		out = append(out, doc...)
		start = end
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "---") {
			flush(i)
		}
	}
	flush(len(lines))
	return strings.Join(out, "")
}

func isSecret(doc []string) bool {
	for _, l := range doc {
		if secretKind.MatchString(strings.TrimRight(l, "\r\n")) {
			return true
		}
	}
	return false
}

// blankData replaces each entry of the top-level data and stringData maps with its key
// and the placeholder, and drops the lines of multi-line values.
func blankData(doc []string) []string {
	out := make([]string, 0, len(doc))
	in, keyIndent := false, -1
	for _, raw := range doc {
		l := strings.TrimRight(raw, "\r\n")
		nl := raw[len(l):]
		if m := secretField.FindStringSubmatch(l); m != nil {
			in, keyIndent = true, -1
			if v := strings.TrimSpace(m[2]); v != "" && !strings.HasPrefix(v, "#") {
				out = append(out, m[1]+": "+Placeholder+nl)
				in = false
				continue
			}
			out = append(out, raw)
			continue
		}
		if !in {
			out = append(out, raw)
			continue
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			in = false
			out = append(out, raw)
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		if keyIndent < 0 && strings.TrimSpace(l) != "" {
			keyIndent = indent
		}
		if m := entry.FindStringSubmatch(l); m != nil && indent == keyIndent {
			out = append(out, m[1]+m[2]+": "+Placeholder+nl)
		}
		// Anything else inside the map is part of a value, or a blank line or comment
		// there, and is left out.
	}
	return out
}
