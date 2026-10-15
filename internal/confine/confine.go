// Package confine rewrites query-language expressions so that every selector stays inside
// a call's scope: its namespace and, where a backend holds several clusters, its cluster.
// See docs/design/0004, "Confining queries".
package confine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

var (
	// ErrOutOfScope is returned for a query that selects outside the call's scope.
	ErrOutOfScope = errors.New("query selects outside the call's scope")
	// ErrNotLogQuery is returned for LogQL that is not a single-selector log query.
	ErrNotLogQuery = errors.New("only log queries are supported: one {stream selector} followed by an optional pipeline")
)

// Scope is what every selector must be restricted to.
type Scope struct {
	NamespaceLabel string
	Namespace      string
	ClusterLabel   string // empty: the backend holds one cluster, no matcher is added
	Cluster        string
}

func (s Scope) required() [][2]string {
	req := [][2]string{{s.NamespaceLabel, s.Namespace}}
	if s.ClusterLabel != "" {
		req = append(req, [2]string{s.ClusterLabel, s.Cluster})
	}
	return req
}

// PromQL adds the scope's matchers to every vector selector of query. A selector with
// another matcher on a scope label is rejected rather than rewritten.
func PromQL(query string, s Scope) (string, error) {
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		return "", fmt.Errorf("invalid PromQL: %w", err)
	}
	var outErr error
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok || outErr != nil {
			return nil
		}
		for _, req := range s.required() {
			present := false
			for _, m := range vs.LabelMatchers {
				if m.Name != req[0] {
					continue
				}
				if m.Type != labels.MatchEqual || m.Value != req[1] {
					outErr = fmt.Errorf("%w: %s has %s, the call is limited to %s=%q", ErrOutOfScope, vs, m, req[0], req[1])
					return nil
				}
				present = true
			}
			if !present {
				m, err := labels.NewMatcher(labels.MatchEqual, req[0], req[1])
				if err != nil {
					outErr = err
					return nil
				}
				vs.LabelMatchers = append(vs.LabelMatchers, m)
			}
		}
		return nil
	})
	if outErr != nil {
		return "", outErr
	}
	return expr.String(), nil
}

type logMatcher struct {
	name, op, literal, value string
}

// LogQL adds the scope's matchers to the stream selector of a log query. Only a single
// selector followed by an optional pipeline is accepted: that is the shape that can be
// confined without a full LogQL parser.
func LogQL(query string, s Scope) (string, error) {
	q := strings.TrimLeft(query, " \t\r\n")
	if !strings.HasPrefix(q, "{") {
		return "", ErrNotLogQuery
	}
	matchers, rest, err := parseSelector(q)
	if err != nil {
		return "", fmt.Errorf("invalid LogQL stream selector: %w", err)
	}
	if hasBraceOutsideStrings(rest) {
		return "", ErrNotLogQuery
	}
	for _, req := range s.required() {
		present := false
		for _, m := range matchers {
			if m.name != req[0] {
				continue
			}
			if m.op != "=" || m.value != req[1] {
				return "", fmt.Errorf("%w: %s%s%s, the call is limited to %s=%q", ErrOutOfScope, m.name, m.op, m.literal, req[0], req[1])
			}
			present = true
		}
		if !present {
			matchers = append(matchers, logMatcher{name: req[0], op: "=", literal: strconv.Quote(req[1])})
		}
	}
	parts := make([]string, len(matchers))
	for i, m := range matchers {
		parts[i] = m.name + m.op + m.literal
	}
	return "{" + strings.Join(parts, ", ") + "}" + rest, nil
}

// parseSelector reads `{name op "value", ...}` at the start of q and returns the
// matchers and what follows the closing brace.
func parseSelector(q string) ([]logMatcher, string, error) {
	i := 1 // past '{'
	var out []logMatcher
	skip := func() {
		for i < len(q) && strings.ContainsRune(" \t\r\n", rune(q[i])) {
			i++
		}
	}
	skip()
	if i < len(q) && q[i] == '}' {
		return out, q[i+1:], nil
	}
	for {
		skip()
		start := i
		for i < len(q) && (q[i] == '_' || isLetter(q[i]) || (i > start && isDigit(q[i]))) {
			i++
		}
		if i == start {
			return nil, "", fmt.Errorf("expected a label name at offset %d", i)
		}
		name := q[start:i]
		skip()
		var op string
		for _, candidate := range []string{"=~", "!~", "!=", "="} {
			if strings.HasPrefix(q[i:], candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return nil, "", fmt.Errorf("expected =, !=, =~ or !~ after %q", name)
		}
		i += len(op)
		skip()
		lit, value, n, err := readString(q[i:])
		if err != nil {
			return nil, "", fmt.Errorf("value of %q: %w", name, err)
		}
		i += n
		out = append(out, logMatcher{name: name, op: op, literal: lit, value: value})
		skip()
		if i >= len(q) {
			return nil, "", errors.New("unterminated stream selector")
		}
		switch q[i] {
		case ',':
			i++
		case '}':
			return out, q[i+1:], nil
		default:
			return nil, "", fmt.Errorf("expected , or } at offset %d", i)
		}
	}
}

// readString reads a double-quoted (with escapes) or backtick string at the start of s
// and returns its literal text, its value and its length.
func readString(s string) (literal, value string, n int, err error) {
	if s == "" {
		return "", "", 0, errors.New("missing string")
	}
	switch s[0] {
	case '`':
		end := strings.IndexByte(s[1:], '`')
		if end < 0 {
			return "", "", 0, errors.New("unterminated string")
		}
		return s[:end+2], s[1 : end+1], end + 2, nil
	case '"':
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				v, err := strconv.Unquote(s[:i+1])
				if err != nil {
					return "", "", 0, err
				}
				return s[:i+1], v, i + 1, nil
			}
		}
		return "", "", 0, errors.New("unterminated string")
	default:
		return "", "", 0, errors.New("values must be quoted")
	}
}

// hasBraceOutsideStrings reports whether s contains '{' outside string literals, which in
// a log query's pipeline can only start another stream selector.
func hasBraceOutsideStrings(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			return true
		case '"', '`':
			_, _, n, err := readString(s[i:])
			if err != nil {
				return true // an unterminated string: refuse rather than guess
			}
			i += n - 1
		}
	}
	return false
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
func isDigit(b byte) bool  { return b >= '0' && b <= '9' }
