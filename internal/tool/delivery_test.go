package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/source"
)

// appWith returns an Application reader serving one Application, argocd/api.
func appWith(status string, resources int) func(context.Context, string, string) (*source.Application, error) {
	var rs []string
	for i := range resources {
		rs = append(rs, fmt.Sprintf(`{"kind":"ConfigMap","namespace":"shop","name":"cm-%02d","status":"OutOfSync"}`, i))
	}
	rs = append(rs, `{"group":"apps","kind":"Deployment","namespace":"shop","name":"api","status":"Synced","health":{"status":"Degraded","message":"token=s3cr3tvalue123 rejected"}}`,
		`{"kind":"Service","namespace":"shop","name":"api","status":"Synced","health":{"status":"Healthy"}}`)
	raw := `{"spec":{"source":{"repoURL":"https://git.example.com/team-a/gitops.git","path":"apps/api","targetRevision":"main"},"destination":{"namespace":"shop"}},
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
	cfg := staticChanges{&Changes{Resolve: source.Options{ApplicationNamespace: "argocd", Applications: apps}}}
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
