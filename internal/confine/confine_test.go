package confine

import (
	"errors"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/core"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

var scope = Scope{NamespaceLabel: "namespace", Namespace: "shop", ClusterLabel: "cluster", Cluster: "prod-a"}

// assertConfined parses the rewritten query independently and checks that every vector
// selector carries exactly the required matchers.
func assertConfined(t *testing.T, q string, s Scope) {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("rewritten query does not parse: %q: %v", q, err)
	}
	selectors := 0
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		selectors++
		need := map[string]string{s.NamespaceLabel: s.Namespace}
		if s.ClusterLabel != "" {
			need[s.ClusterLabel] = s.Cluster
		}
		for _, m := range vs.LabelMatchers {
			if want, ok := need[m.Name]; ok {
				if m.Type != labels.MatchEqual || m.Value != want {
					t.Errorf("%q: selector %s has %s", q, vs, m)
				}
				delete(need, m.Name)
			}
		}
		if len(need) > 0 {
			t.Errorf("%q: selector %s lacks %v", q, vs, need)
		}
		return nil
	})
	_ = selectors
}

func TestPromQLConfinesEverySelector(t *testing.T) {
	for _, q := range []string{
		`up`,
		`rate(http_requests_total{code="500"}[5m])`,
		`max_over_time(rate(http_requests_total[1m])[1h:5m])`,
		`sum by (pod) (container_memory_working_set_bytes) / on (pod) group_left sum by (pod) (kube_pod_container_resource_limits{resource="memory"})`,
		`absent(nonexistent_metric)`,
		`{__name__=~".+"}`,
		`label_replace(up, "namespace", "other", "", "")`,
		`up offset 1h`,
		`histogram_quantile(0.9, sum by (le) (rate(request_duration_seconds_bucket[5m])))`,
		`count(up) > bool 1 or vector(0)`,
		`up{namespace="shop"}`,
		`up{namespace="shop", cluster="prod-a"}`,
	} {
		t.Run(q, func(t *testing.T) {
			got, err := PromQL(q, scope)
			if err != nil {
				t.Fatalf("PromQL(%q): %v", q, err)
			}
			assertConfined(t, got, scope)
			if strings.Count(got, `namespace="shop"`) == 0 {
				t.Fatalf("no namespace matcher in %q", got)
			}
		})
	}
}

func TestPromQLDoesNotDuplicateMatchers(t *testing.T) {
	got, err := PromQL(`up{namespace="shop"}`, scope)
	if err != nil || strings.Count(got, `namespace="shop"`) != 1 {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPromQLWithoutClusterLabel(t *testing.T) {
	s := Scope{NamespaceLabel: "namespace", Namespace: "shop"}
	got, err := PromQL(`up`, s)
	if err != nil || strings.Contains(got, "cluster") {
		t.Fatalf("got %q, %v", got, err)
	}
	assertConfined(t, got, s)
}

func TestPromQLRejectsQueriesOutsideScope(t *testing.T) {
	for _, q := range []string{
		`up{namespace="other"}`,
		`up{namespace!="shop"}`,
		`up{namespace=~"shop|other"}`,
		`up{namespace=~"shop"}`,
		`up{namespace=""}`,
		`rate(x{namespace="other"}[5m]) + rate(x[5m])`,
		`up{cluster="prod-b"}`,
		`up{cluster=~".+"}`,
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := PromQL(q, scope); !errors.Is(err, ErrOutOfScope) {
				t.Fatalf("PromQL(%q) err = %v, want ErrOutOfScope", q, err)
			}
		})
	}
}

func TestPromQLRejectsInvalidAndExperimental(t *testing.T) {
	for _, q := range []string{`up{`, `sum(`, ``, `info(up)`} {
		if _, err := PromQL(q, scope); err == nil {
			t.Errorf("PromQL(%q) accepted", q)
		}
	}
}

func TestPromQLWithoutSelectors(t *testing.T) {
	got, err := PromQL(`vector(1)`, scope)
	if err != nil || got != `vector(1)` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLogQLConfinesTheSelector(t *testing.T) {
	for in, want := range map[string]string{
		`{app="api"}`: `{app="api", namespace="shop", cluster="prod-a"}`,
		`{app="api"} |= "error" | json | line_format "{{.msg}}"`: `{app="api", namespace="shop", cluster="prod-a"} |= "error" | json | line_format "{{.msg}}"`,
		"{app=~`api.*`}":                    "{app=~`api.*`, namespace=\"shop\", cluster=\"prod-a\"}",
		`{app="a\"b}"}`:                     `{app="a\"b}", namespace="shop", cluster="prod-a"}`,
		`  {app="api",container!="istio"} `: `{app="api", container!="istio", namespace="shop", cluster="prod-a"} `,
		`{namespace="shop"} |~ "x{2}"`:      `{namespace="shop", cluster="prod-a"} |~ "x{2}"`,
		"{app=\"api\"} |~ `a{1,3}`":         "{app=\"api\", namespace=\"shop\", cluster=\"prod-a\"} |~ `a{1,3}`",
		`{}`:                                `{namespace="shop", cluster="prod-a"}`,
	} {
		t.Run(in, func(t *testing.T) {
			got, err := LogQL(in, scope)
			if err != nil || got != want {
				t.Fatalf("LogQL(%q) = %q, %v; want %q", in, got, err, want)
			}
		})
	}
}

func TestLogQLRejects(t *testing.T) {
	for q, want := range map[string]error{
		`rate({app="x"}[5m])`:                  ErrNotLogQuery,
		`count_over_time({app="x"}[1m])`:       ErrNotLogQuery,
		`sum by (pod) (rate({app="x"}[5m]))`:   ErrNotLogQuery,
		`{app="x"} or {app="y"}`:               ErrNotLogQuery,
		`{app="x"} |= "a" {namespace="other"}`: ErrNotLogQuery,
		`{namespace="other"}`:                  ErrOutOfScope,
		`{namespace=~".*"}`:                    ErrOutOfScope,
		`{namespace!="shop"}`:                  ErrOutOfScope,
		`{cluster="prod-b"}`:                   ErrOutOfScope,
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := LogQL(q, scope); !errors.Is(err, want) {
				t.Fatalf("LogQL(%q) err = %v, want %v", q, err, want)
			}
		})
	}
	for _, q := range []string{`{app="x"`, `{app=x}`, `{1app="x"}`, `{app="x" app2="y"}`, `{app="x}`, `{app=="x"}`} {
		if _, err := LogQL(q, scope); err == nil {
			t.Errorf("malformed %q accepted", q)
		}
	}
}

// A comment (# to end of line) must not hide a second selector from the scanner: a
// backtick inside a comment is not a string. Loki would reject the result anyway; this
// keeps confinement from depending on that.
func TestLogQLCommentsCannotHideASelector(t *testing.T) {
	for _, q := range []string{
		"{app=\"x\"} # `\n{namespace=\"other\"}\n# `",
		"{app=\"x\"} # \"\n{namespace=\"other\"}\n# \"",
	} {
		if _, err := LogQL(q, scope); !errors.Is(err, ErrNotLogQuery) {
			t.Errorf("LogQL(%q) err = %v, want ErrNotLogQuery", q, err)
		}
	}
	// A brace inside a comment is harmless and accepted.
	if _, err := LogQL("{app=\"x\"} # a comment with { braces }\n|= \"y\"", scope); err != nil {
		t.Fatalf("brace in a comment: %v", err)
	}
}

func TestOutOfScopeIsInvalidInput(t *testing.T) {
	_, err := PromQL(`up{namespace="other"}`, Scope{NamespaceLabel: "namespace", Namespace: "shop"})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want it to be invalid input", err)
	}
	if _, err := LogQL(`rate({a="b"}[5m])`, Scope{NamespaceLabel: "namespace", Namespace: "shop"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("metric LogQL: %v", err)
	}
	if _, err := PromQL(`up{`, Scope{NamespaceLabel: "namespace", Namespace: "shop"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("syntax error: %v", err)
	}
}
