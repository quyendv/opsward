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
}

func TestListWorkloadsItemsCarryLabelsForPolicy(t *testing.T) {
	src := &fakeSource{summaries: []capability.WorkloadSummary{
		{Namespace: "shop", Kind: "Deployment", Name: "api", Desired: 2, Ready: 1, Labels: map[string]string{"app.kubernetes.io/part-of": "checkout"}},
	}}
	tl, err := ListWorkloads(testEnvs, src)
	if err != nil {
		t.Fatal(err)
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

// Without a namespace, list_workloads spans the environment's namespaces; unhealthy keeps
// only workloads with fewer ready replicas than desired.
func TestListWorkloadsAcrossNamespacesAndUnhealthy(t *testing.T) {
	src := &fakeSource{namespaces: []string{"kube-system", "shared", "team-a-api"}, summaries: []capability.WorkloadSummary{
		{Namespace: "kube-system", Kind: "DaemonSet", Name: "proxy", Desired: 1, Ready: 1},
		{Namespace: "shared", Kind: "Deployment", Name: "web", Desired: 2, Ready: 2},
		{Namespace: "team-a-api", Kind: "Deployment", Name: "api", Desired: 2, Ready: 1},
	}}
	tl, err := ListWorkloads(testEnvs, src)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := tl.Prepare(json.RawMessage(`{"environment":"staging"}`))
	if err != nil {
		t.Fatal(err)
	}
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Scope.Namespace+"/"+it.Scope.Name)
	}
	if strings.Join(got, ",") != "shared/web,team-a-api/api" {
		t.Fatalf("items = %v; kube-system is outside the environment", got)
	}
	// An environment limited to some namespaces is listed namespace by namespace, so a
	// reader bound only in those namespaces can list it.
	if strings.Join(src.calls, " ") != "namespaces workloads:shared workloads:team-a-api" {
		t.Fatalf("calls = %v", src.calls)
	}
	prep, _ = tl.Prepare(json.RawMessage(`{"environment":"staging","unhealthy":true}`))
	items, _ = prep.RunList(context.Background(), "reader")
	if len(items) != 1 || items[0].Scope.Name != "api" {
		t.Fatalf("unhealthy = %+v", items)
	}
}

func TestListWorkloadsInAnUnlimitedEnvironmentListsOnce(t *testing.T) {
	src := &fakeSource{summaries: []capability.WorkloadSummary{{Namespace: "a", Kind: "Deployment", Name: "x", Desired: 1, Ready: 1}}}
	tl, _ := ListWorkloads(testEnvs, src)
	prep, _ := tl.Prepare(json.RawMessage(`{"environment":"production"}`))
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil || len(items) != 1 || strings.Join(src.calls, " ") != "workloads:" {
		t.Fatalf("items %+v, calls %v, %v", items, src.calls, err)
	}
}
