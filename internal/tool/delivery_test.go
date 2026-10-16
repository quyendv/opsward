package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/source"
)

var inCluster = map[string][]string{"staging": {"https://kubernetes.default.svc"}}

// appWith returns an Application reader serving one Application, argocd/api.
func appWith(status string, resources int) func(context.Context, string, string) (*source.Application, error) {
	var rs []string
	for i := range resources {
		rs = append(rs, fmt.Sprintf(`{"kind":"ConfigMap","namespace":"shop","name":"cm-%02d","status":"OutOfSync"}`, i))
	}
	rs = append(rs, `{"group":"apps","kind":"Deployment","namespace":"shop","name":"api","status":"Synced","health":{"status":"Degraded","message":"token=s3cr3tvalue123 rejected"}}`,
		`{"kind":"Service","namespace":"shop","name":"api","status":"Synced","health":{"status":"Healthy"}}`)
	raw := `{"spec":{"source":{"repoURL":"https://git.example.com/team-a/gitops.git","path":"apps/api","targetRevision":"main"},"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
		"status":{"sync":{"status":"` + status + `","revision":"abc123"},"health":{"status":"Degraded","message":"1 pod crashing"},
		"operationState":{"phase":"Failed","message":"one or more objects failed to apply","startedAt":"2026-10-16T08:00:00Z","finishedAt":"2026-10-16T08:00:05Z","syncResult":{"revision":"abc123"}},
		"conditions":[{"type":"SyncError","message":"Failed sync attempt"}],
		"resources":[` + strings.Join(rs, ",") + `],
		"history":[{"revision":"r1","deployedAt":"2026-10-10T08:00:00Z"},{"revision":"r2","deployedAt":"2026-10-11T08:00:00Z"},{"revision":"r3","deployedAt":"2026-10-12T08:00:00Z"},
			{"revision":"r4","deployedAt":"2026-10-13T08:00:00Z"},{"revision":"r5","deployedAt":"2026-10-14T08:00:00Z"},{"revision":"abc123","deployedAt":"2026-10-16T08:00:05Z"}]}}`
	return func(_ context.Context, ns, name string) (*source.Application, error) {
		if ns+"/"+name != "argocd/api" {
			return nil, source.ErrNoApplication
		}
		var a source.Application
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		return &a, nil
	}
}

func deliveryTool(t *testing.T, annotations map[string]string, apps func(context.Context, string, string) (*source.Application, error)) *DeliveryView {
	t.Helper()
	src := &fakeSource{workload: &capability.Workload{}, annotations: annotations}
	cfg := staticChanges{&Changes{Resolve: source.Options{ApplicationNamespace: "argocd", Applications: apps, Destinations: inCluster}}}
	tl, err := DeliveryStatus(src, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Risk != core.RiskRead || len(tl.Needs) != 1 {
		t.Fatalf("tool = %+v", tl)
	}
	p, err := tl.Prepare(json.RawMessage(`{` + target + `}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	return out.(*DeliveryView)
}

func TestDeliveryStatus(t *testing.T) {
	v := deliveryTool(t, map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}, appWith("OutOfSync", 60))
	if !v.Managed || v.Application != "argocd/api" || v.Sync.Status != "OutOfSync" || v.Sync.Revision != "abc123" || v.Sync.TargetRevision != "main" {
		t.Fatalf("view = %+v", v)
	}
	if v.Health.Status != "Degraded" || v.Operation == nil || v.Operation.Phase != "Failed" || v.Operation.Revision != "abc123" || len(v.Conditions) != 1 {
		t.Fatalf("view = %+v", v)
	}
	// 60 out-of-sync ConfigMaps and one degraded Deployment; the healthy Service is left out.
	if r := v.Resources[0]; r.Name != "api" || r.Health != "Degraded" {
		t.Fatalf("the unhealthy Deployment is not first: %+v", r)
	}
	if len(v.Resources) != 50 || v.ResourcesOmitted != 11 {
		t.Fatalf("resources %d, omitted %d", len(v.Resources), v.ResourcesOmitted)
	}
	if len(v.History) != 5 || v.History[0].Revision != "abc123" || v.History[4].Revision != "r2" {
		t.Fatalf("history = %+v", v.History)
	}
}

func TestDeliveryStatusOfAnUnmanagedWorkload(t *testing.T) {
	v := deliveryTool(t, nil, appWith("Synced", 0))
	if v.Managed || !strings.Contains(v.Note, "no Argo CD Application") {
		t.Fatalf("view = %+v", v)
	}
}

// staticApp serves one raw Application as argocd/api.
func staticApp(raw string) func(context.Context, string, string) (*source.Application, error) {
	return func(_ context.Context, ns, name string) (*source.Application, error) {
		if ns+"/"+name != "argocd/api" {
			return nil, source.ErrNoApplication
		}
		var a source.Application
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		return &a, nil
	}
}

var tracked = map[string]string{"argocd.argoproj.io/tracking-id": "api:apps/Deployment:shop/api"}

// The caller is authorized on one workload: what the Application manages in other
// namespaces, or cluster-wide, is counted but not named, and conditions naming other
// Applications are left out.
func TestDeliveryStatusStaysInTheNamespace(t *testing.T) {
	v := deliveryTool(t, tracked, staticApp(`{"spec":{"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
		"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Degraded"},
		"conditions":[{"type":"SharedResourceWarning","message":"Deployment/api is part of applications argocd/api and payments-app"}],
		"resources":[{"kind":"Secret","namespace":"payments","name":"db","status":"OutOfSync"},{"kind":"ClusterRole","name":"admin-ish","status":"OutOfSync"},
			{"group":"serving.knative.dev","kind":"Service","namespace":"shop","name":"api","status":"OutOfSync"}]}}`))
	if len(v.Resources) != 1 || v.Resources[0].Group != "serving.knative.dev" || v.ResourcesElsewhere != 2 || len(v.Conditions) != 0 {
		t.Fatalf("view = %+v", v)
	}
}

// Multi-source Applications report revisions per source.
func TestDeliveryStatusOfAMultiSourceApplication(t *testing.T) {
	v := deliveryTool(t, tracked, staticApp(`{"spec":{"sources":[{"repoURL":"https://charts.example.com","chart":"app","targetRevision":"1.2.0"},{"repoURL":"https://git.example.com/team-a/gitops.git","targetRevision":"main","ref":"values"}],
		"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
		"status":{"sync":{"status":"Synced","revisions":["1.2.0","abc123"]},"health":{"status":"Healthy"},
		"operationState":{"phase":"Succeeded","syncResult":{"revisions":["1.2.0","abc123"]}},
		"history":[{"revisions":["1.1.0","9f9f9f9"],"deployedAt":"2026-10-15T08:00:00Z"},{"revisions":["1.2.0","abc123"],"deployedAt":"2026-10-16T08:00:00Z"}]}}`))
	if !slices.Equal(v.Sync.Revisions, []string{"1.2.0", "abc123"}) || !slices.Equal(v.Sync.TargetRevisions, []string{"1.2.0", "main"}) ||
		!slices.Equal(v.Operation.Revisions, []string{"1.2.0", "abc123"}) || !slices.Equal(v.History[0].Revisions, []string{"1.2.0", "abc123"}) || v.History[0].Revision != "" {
		t.Fatalf("view = %+v / %+v / %+v", v.Sync, v.Operation, v.History)
	}
}

// Argo CD 3 keeps resource health out of the Application by default; the answer says so
// rather than look all healthy.
func TestDeliveryStatusSaysWhenResourceHealthIsNotRecorded(t *testing.T) {
	v := deliveryTool(t, tracked, staticApp(`{"spec":{"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
		"status":{"sync":{"status":"Synced"},"health":{"status":"Degraded"},"resourceHealthSource":"appTree","resources":[{"group":"apps","kind":"Deployment","namespace":"shop","name":"api","status":"Synced"}]}}`))
	if !strings.Contains(v.Note, "health") {
		t.Fatalf("note = %q", v.Note)
	}
}

// Render errors and failed syncs can run to kilobytes: each message is cut, and so is
// the number of conditions.
func TestDeliveryStatusBoundsMessages(t *testing.T) {
	long := strings.Repeat("x", 5000)
	var conds []string
	for range 20 {
		conds = append(conds, `{"type":"ComparisonError","message":"`+long+`"}`)
	}
	v := deliveryTool(t, tracked, staticApp(`{"spec":{"destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
		"status":{"sync":{"status":"Unknown"},"health":{"status":"Missing","message":"`+long+`"},"operationState":{"phase":"Error","message":"`+long+`"},
		"conditions":[`+strings.Join(conds, ",")+`]}}`))
	if len(v.Health.Message) > 1100 || len(v.Operation.Message) > 1100 || len(v.Conditions) != 10 || len(v.Conditions[0].Message) > 1100 ||
		!strings.HasSuffix(v.Operation.Message, "(truncated)") || v.ConditionsOmitted != 10 {
		t.Fatalf("health %d, operation %d, conditions %d (omitted %d)", len(v.Health.Message), len(v.Operation.Message), len(v.Conditions), v.ConditionsOmitted)
	}
}
