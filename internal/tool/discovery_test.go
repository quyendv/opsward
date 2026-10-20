package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// A profile that acts as its caller (docs/design/0014) may list some of an environment's
// namespaces and not others: the listing keeps what the backend lets it read.
func TestListWorkloadsSkipsNamespacesTheBackendRefuses(t *testing.T) {
	src := &fakeSource{namespaces: []string{"shared", "team-a-api"}, forbidden: map[string]bool{"team-a-api": true}, summaries: []capability.WorkloadSummary{
		{Namespace: "shared", Kind: "Deployment", Name: "web", Desired: 2, Ready: 2},
		{Namespace: "team-a-api", Kind: "Deployment", Name: "api", Desired: 2, Ready: 1},
	}}
	tl, _ := ListWorkloads(testEnvs, src)
	prep, _ := tl.Prepare(json.RawMessage(`{"environment":"staging"}`))
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Scope.Name != "web" {
		t.Fatalf("items = %+v, want shared/web alone", items)
	}
	// Refused everywhere is still an error, not an empty environment.
	src.forbidden["shared"] = true
	if _, err := prep.RunList(context.Background(), "reader"); !errors.Is(err, capability.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	// A namespace the caller named is not skipped.
	src.forbidden = map[string]bool{"team-a-api": true}
	prep, _ = tl.Prepare(json.RawMessage(`{"environment":"staging","namespace":"team-a-api"}`))
	if _, err := prep.RunList(context.Background(), "reader"); !errors.Is(err, capability.ErrForbidden) {
		t.Fatalf("named namespace: err = %v, want ErrForbidden", err)
	}
}

// restarting keeps the workloads whose containers restarted, most restarts first, with
// the count and when the last one ended; it reads restarts where it listed workloads.
func TestListWorkloadsRestarting(t *testing.T) {
	last := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	src := &fakeSource{
		namespaces: []string{"shared", "team-a-api"},
		summaries: []capability.WorkloadSummary{
			{Namespace: "shared", Kind: "Deployment", Name: "web", Desired: 2, Ready: 2},
			{Namespace: "shared", Kind: "Deployment", Name: "quiet", Desired: 1, Ready: 1},
			{Namespace: "team-a-api", Kind: "Deployment", Name: "api", Desired: 2, Ready: 2},
		},
		restarts: []capability.WorkloadRestarts{
			{Namespace: "shared", Kind: "Deployment", Name: "web", Restarts: 2, LastRestart: &last},
			{Namespace: "team-a-api", Kind: "Deployment", Name: "api", Restarts: 9, LastRestart: &last},
		},
	}
	tl, _ := ListWorkloads(testEnvs, src)
	prep, err := tl.Prepare(json.RawMessage(`{"environment":"staging","restarting":true}`))
	if err != nil {
		t.Fatal(err)
	}
	items, err := prep.RunList(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		s := it.Data.(capability.WorkloadSummary)
		got = append(got, fmt.Sprintf("%s %d %v", s.Name, s.Restarts, s.LastRestart != nil && s.LastRestart.Equal(last)))
	}
	if strings.Join(got, ",") != "api 9 true,web 2 true" {
		t.Fatalf("items = %v", got)
	}
	if strings.Join(src.calls, " ") != "namespaces workloads:shared workloads:team-a-api restarts:shared restarts:team-a-api" {
		t.Fatalf("calls = %v", src.calls)
	}
	// Refused pods everywhere is an error, not a cluster where nothing restarts.
	src.calls, src.restartsErr = nil, fmt.Errorf("list pods: %w", capability.ErrForbidden)
	if _, err := prep.RunList(context.Background(), "reader"); !errors.Is(err, capability.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	src.restartsErr = nil
	// Without restarting, a listing reads no pods.
	src.calls = nil
	prep, _ = tl.Prepare(json.RawMessage(`{"environment":"staging"}`))
	if items, _ = prep.RunList(context.Background(), "reader"); len(items) != 3 || slices.ContainsFunc(src.calls, func(c string) bool { return strings.HasPrefix(c, "restarts") }) {
		t.Fatalf("%d items, calls %v", len(items), src.calls)
	}
}
