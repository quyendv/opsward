package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
)

type fixedEnvironments []EnvironmentInfo

func (f fixedEnvironments) Environments() []EnvironmentInfo { return f }

var testEnvs = fixedEnvironments{
	{Name: "staging", Cluster: "cluster-a", Namespaces: []string{"team-a-*", "shared", "typo-ns"}, Metrics: true},
	{Name: "production", Cluster: "cluster-b", Namespaces: []string{"*"}},
}

func TestListEnvironmentsItems(t *testing.T) {
	tl, err := ListEnvironments(testEnvs)
	if err != nil {
		t.Fatal(err)
	}
	if !tl.List || !tl.Global {
		t.Fatalf("list_environments must be a global listing: %+v", tl)
	}
	prep, err := tl.Prepare(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil || len(items) != 2 || !reflect.DeepEqual(items[0].Scope, core.Scope{Environment: "staging"}) {
		t.Fatalf("items = %+v, %v", items, err)
	}
	if e := items[0].Data.(EnvironmentInfo); e.Cluster != "cluster-a" || !e.Metrics || e.Logs {
		t.Fatalf("data = %+v", e)
	}
}

func TestListNamespacesFiltersByEnvironmentPatterns(t *testing.T) {
	src := &fakeSource{namespaces: []string{"kube-system", "shared", "team-a-api", "team-a-web", "team-b"}}
	tl, err := ListNamespaces(testEnvs, src)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := tl.Prepare(json.RawMessage(`{"environment":"staging"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prep.Scope, core.Scope{Environment: "staging"}) {
		t.Fatalf("scope = %+v", prep.Scope)
	}
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		if it.Scope.Environment != "staging" || it.Scope.Namespace != it.Data {
			t.Fatalf("item = %+v", it)
		}
		names = append(names, it.Data.(string))
	}
	if strings.Join(names, ",") != "shared,team-a-api,team-a-web" {
		t.Fatalf("namespaces = %v", names)
	}
	out, _ := json.Marshal(prep.Wrap([]any{"shared"}))
	if string(out) != `{"namespaces":["shared"],"unmatchedPatterns":["typo-ns"]}` {
		t.Fatalf("result = %s", out)
	}
}

func TestListWorkloadsItemsCarryLabelsForPolicy(t *testing.T) {
	src := &fakeSource{summaries: []capability.WorkloadSummary{
		{Kind: "Deployment", Name: "api", Desired: 2, Ready: 1, Labels: map[string]string{"app.kubernetes.io/part-of": "checkout"}},
	}}
	tl, err := ListWorkloads(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Prepare(json.RawMessage(`{"environment":"staging"}`)); err == nil {
		t.Fatal("namespace is required")
	}
	prep, err := tl.Prepare(json.RawMessage(`{"environment":"staging","namespace":"shop"}`))
	if err != nil {
		t.Fatal(err)
	}
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v, %v", items, err)
	}
	sc := items[0].Scope
	if sc.Namespace != "shop" || sc.Kind != "Deployment" || sc.Name != "api" || !sc.LabelsKnown || sc.Labels["app.kubernetes.io/part-of"] != "checkout" {
		t.Fatalf("scope = %+v", sc)
	}
	out, _ := json.Marshal(items[0].Data)
	if strings.Contains(string(out), "checkout") {
		t.Fatalf("labels returned: %s", out)
	}
}
