package config

import (
	"strings"
	"testing"
)

const forges = `
  forges:
    - name: github
      type: github
      url: https://api.github.com
      hosts: [github.com]
      token: { file: /etc/opsward/github-token }
      repositories: [example-org/gitops]
`

// withChange adds forges and a delivery section to minimal's providers, and rest at the
// end of the document.
func withChange(providers, rest string) string {
	return strings.Replace(minimal, "environments:\n", strings.TrimPrefix(providers, "\n")+"environments:\n", 1) + rest
}

func TestChangeSectionParses(t *testing.T) {
	doc := withChange(forges+"  delivery: { type: argocd, cluster: cluster-a }\n",
		"    change: { strategy: pull-request }\nsourceResolution:\n  static:\n    - { environment: staging, namespace: shop, name: api, source: \"github:example-org/gitops//apps/api@main\" }\n")
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := c.Providers.Delivery
	if d.Namespace != "argocd" || d.Profile != "reader" {
		t.Errorf("delivery defaults not applied: %+v", d)
	}
	if got := c.SourceResolution.Annotations; len(got) != 1 || got[0] != "opsward/source" {
		t.Errorf("sourceResolution.annotations = %v, want [opsward/source]", got)
	}
	if c.Environments[0].Change.Strategy != "pull-request" || c.Providers.Forges[0].Token.File == "" {
		t.Errorf("parsed = %+v", c)
	}
}

func TestChangeSectionRejects(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"pull-request without a forge", minimal + "    change: { strategy: pull-request }\n", "needs a forge"},
		{"unknown strategy", minimal + "    change: { strategy: direct }\n", `change strategy "direct" is not available`},
		{"unknown forge type", withChange(strings.Replace(forges, "type: github", "type: bitbucket", 1), ""), `forge "github": type "bitbucket"`},
		{"inline token", withChange(strings.Replace(forges, "{ file: /etc/opsward/github-token }", "ghp_x", 1), ""), "an inline value is not accepted"},
		{"no repositories", withChange(strings.Replace(forges, "      repositories: [example-org/gitops]\n", "", 1), ""), "repositories is required"},
		{"bad repository", withChange(strings.Replace(forges, "[example-org/gitops]", "[/gitops]", 1), ""), `repository "/gitops"`},
		{"duplicate forge", withChange(forges+strings.TrimPrefix(forges, "\n  forges:\n"), ""), `forge "github": duplicate name`},
		{"credentials in url", withChange(strings.Replace(forges, "https://api.github.com", "https://u:p@api.github.com", 1), ""), "must not contain credentials"},
		{"delivery unknown cluster", withChange(forges+"  delivery: { type: argocd, cluster: cluster-z }\n", ""), `delivery: unknown cluster "cluster-z"`},
		{"delivery unknown profile", withChange(forges+"  delivery: { type: argocd, cluster: cluster-a, profile: argo }\n", ""), `delivery: cluster "cluster-a" has no profile "argo"`},
		{"delivery type", withChange(forges+"  delivery: { type: flux, cluster: cluster-a }\n", ""), `delivery.type: "flux"`},
		{"static unknown environment", withChange(forges, "sourceResolution:\n  static:\n    - { environment: prod, namespace: shop, name: api, source: \"github:example-org/gitops//apps/api\" }\n"), `unknown environment "prod"`},
		{"static bad source", withChange(forges, "sourceResolution:\n  static:\n    - { environment: staging, namespace: shop, name: api, source: \"apps/api\" }\n"), "want <repository>//<path>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestForgesAndDeliveryAreRestartOnly(t *testing.T) {
	a, err := Parse([]byte(withChange(forges, "")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(withChange(strings.Replace(forges, "[example-org/gitops]", "[example-org/gitops, example-org/apps]", 1), "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.RestartRequired(b); len(got) != 1 || got[0] != "providers.forges" {
		t.Fatalf("RestartRequired = %v, want [providers.forges]", got)
	}
}

func TestForgeWarnings(t *testing.T) {
	doc := withChange(strings.Replace(strings.Replace(forges, "https://api.github.com", "http://git.example.com", 1), "/etc/opsward/github-token", "/nonexistent/token", 1), "")
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{`forge "github" sends its token over plain http`, `forge "github": token.file does not exist here`} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings miss %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "/nonexistent/token") {
		t.Errorf("warnings name a credential path:\n%s", w)
	}
}
