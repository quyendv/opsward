package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/engine"
)

func schemaJSON(t *testing.T, tl engine.Tool) string {
	t.Helper()
	b, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorkloadEventsTool(t *testing.T) {
	src := &fakeSource{
		workload: &capability.Workload{Labels: map[string]string{"app.kubernetes.io/part-of": "shop"}},
		events:   []capability.Event{{Reason: "FailedScheduling", Object: "Pod/api-a"}},
	}
	tl, err := WorkloadEvents(src)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name != "list_workload_events" || !strings.Contains(schemaJSON(t, tl), `"limit"`) {
		t.Fatalf("tool %s schema %s", tl.Name, schemaJSON(t, tl))
	}
	for _, bad := range []string{
		`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","limit":0}`,
		`{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","limit":201}`,
		`{"environment":"staging","namespace":"shop","kind":"Pod","name":"api"}`,
	} {
		if _, err := tl.Prepare(json.RawMessage(bad)); err == nil {
			t.Errorf("Prepare(%s) succeeded", bad)
		}
	}
	p, err := tl.Prepare(json.RawMessage(validArgs))
	if err != nil {
		t.Fatal(err)
	}
	if labels, err := p.Resolve(context.Background()); err != nil || labels["app.kubernetes.io/part-of"] != "shop" {
		t.Fatalf("Resolve = %v, %v", labels, err)
	}
	out, err := p.Run(context.Background(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "FailedScheduling") || src.profiles[len(src.profiles)-1] != "team-a" {
		t.Fatalf("Run = %s, profiles %v", b, src.profiles)
	}
}

func TestWorkloadEventsDefaultLimit(t *testing.T) {
	var limits []int
	tl, err := WorkloadEvents(&limitRecorder{fakeSource: &fakeSource{workload: &capability.Workload{}}, limits: &limits})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{validArgs, strings.Replace(validArgs, `"name":"api"`, `"name":"api","limit":7`, 1)} {
		p, err := tl.Prepare(json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Run(context.Background(), engine.ProfileReader); err != nil {
			t.Fatal(err)
		}
	}
	if len(limits) != 2 || limits[0] != 50 || limits[1] != 7 {
		t.Fatalf("limits = %v, want [50 7]", limits)
	}
}

type limitRecorder struct {
	*fakeSource
	limits *[]int
}

func (l *limitRecorder) Workloads(env, profile string) (capability.Workloads, error) { return l, nil }

func (l *limitRecorder) WorkloadEvents(ctx context.Context, ref capability.WorkloadRef, limit int) ([]capability.Event, error) {
	*l.limits = append(*l.limits, limit)
	return nil, nil
}

func TestWorkloadLogsTool(t *testing.T) {
	src := &fakeSource{
		workload: &capability.Workload{},
		logs:     &capability.WorkloadLogs{Logs: []capability.ContainerLog{{Pod: "api-a", Container: "app", Lines: []string{"boom"}}}},
	}
	tl, err := WorkloadLogs(src)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name != "list_workload_logs" {
		t.Fatal(tl.Name)
	}
	for _, bad := range []string{
		strings.Replace(validArgs, `"name":"api"`, `"name":"api","tailLines":1001`, 1),
		strings.Replace(validArgs, `"name":"api"`, `"name":"api","tailLines":0`, 1),
		strings.Replace(validArgs, `"name":"api"`, `"name":"api","since":"1h"`, 1),
	} {
		if _, err := tl.Prepare(json.RawMessage(bad)); err == nil {
			t.Errorf("Prepare(%s) succeeded", bad)
		}
	}

	p, err := tl.Prepare(json.RawMessage(validArgs))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), engine.ProfileReader)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.logOptions; got != (capability.LogOptions{TailLines: 100, MaxPods: 10, MaxBytes: 256 << 10, TotalBytes: 1 << 20}) {
		t.Fatalf("defaults = %+v", got)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), "boom") {
		t.Fatalf("Run = %s", b)
	}

	p, err = tl.Prepare(json.RawMessage(strings.Replace(validArgs, `"name":"api"`, `"name":"api","pod":"api-7d9f-a","container":"app","tailLines":20,"previous":true`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), engine.ProfileReader); err != nil {
		t.Fatal(err)
	}
	if got := src.logOptions; got.Pod != "api-7d9f-a" || got.Container != "app" || got.TailLines != 20 || !got.Previous {
		t.Fatalf("options = %+v", got)
	}
}

func TestWorkloadLogsCutLongLines(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{}, logs: &capability.WorkloadLogs{Logs: []capability.ContainerLog{{Pod: "a", Container: "c", Lines: []string{strings.Repeat("z", 9000)}}}}}
	tl, _ := WorkloadLogs(src)
	_, out, err := runTool(t, tl, `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}`)
	if err != nil {
		t.Fatal(err)
	}
	if l := out.(*capability.WorkloadLogs).Logs[0].Lines[0]; len(l) > maxLogLineChars+64 {
		t.Fatalf("line of %d chars", len(l))
	}
}
