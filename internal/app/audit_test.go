package app

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/config"
)

// searchAudit returns the tools and namespaces of the records search_audit keeps for the
// fixture's principal.
func (f *fixture) searchAudit() []string {
	f.t.Helper()
	res, err := f.app.Engine.Invoke(context.Background(), f.app.Principal, "search_audit", json.RawMessage(`{}`))
	if err != nil {
		f.t.Fatalf("search_audit: %v", err)
	}
	b, _ := json.Marshal(res.Data)
	var out struct {
		Records []struct {
			Tool  string `json:"tool"`
			Scope struct {
				Namespace string `json:"namespace"`
			} `json:"scope"`
		} `json:"records"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		f.t.Fatal(err)
	}
	var got []string
	for _, r := range out.Records {
		got = append(got, r.Tool+"@"+r.Scope.Namespace)
	}
	slices.Sort(got)
	return slices.Compact(got)
}

// reopenWith opens a new App on the fixture's configuration with an auditor binding.
func (f *fixture) reopenWith(binding string) {
	f.t.Helper()
	_ = f.app.Close()
	doc := strings.Replace(f.doc("team-a"), "    viewer: { risk: [read] }\n", "    viewer: { risk: [read] }\n    auditor: { tools: [search_audit] }\n", 1)
	if err := os.WriteFile(f.path, []byte(doc+binding), 0o600); err != nil {
		f.t.Fatal(err)
	}
	cfg, err := config.Load(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.app = open(f.t, f, cfg)
}

func TestSearchAuditKeepsRecordsPolicyGrants(t *testing.T) {
	f := newFixture(t, "team-a")
	_ = f.call("get_workload_status", statusArgs) // staging/shop; fails on the kubeconfig, and is recorded
	_ = f.call("get_workload_status", `{"environment":"staging","namespace":"other","kind":"Deployment","name":"api"}`)
	_ = f.call("list_environments", `{}`)

	// A viewer's risk class does not grant search_audit.
	if got := f.searchAudit(); len(got) != 0 {
		t.Fatalf("a viewer found %v", got)
	}

	f.reopenWith("    - name: audit-shop\n      subjects: { groups: [team-a] }\n      role: auditor\n      scope: { namespaces: [shop] }\n")
	if got := f.searchAudit(); !slices.Equal(got, []string{"get_workload_status@shop"}) {
		t.Fatalf("an auditor of shop found %v", got)
	}

	f.reopenWith("    - name: audit-all\n      subjects: { groups: [team-a] }\n      role: auditor\n")
	got := f.searchAudit()
	for _, want := range []string{"get_workload_status@shop", "get_workload_status@other", "list_environments@", "search_audit@"} {
		if !slices.Contains(got, want) {
			t.Fatalf("an unrestricted auditor found %v, want %s among them", got, want)
		}
	}
}
