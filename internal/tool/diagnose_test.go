package tool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
)

func failingWorkload() *capability.Workload {
	return &capability.Workload{Kind: "Deployment", Name: "api", Pods: []capability.Pod{
		{Name: "api-ok", Phase: "Running", Ready: true},
		{
			Name: "api-bad", Phase: "Running", Restarts: 4, Waiting: "CrashLoopBackOff",
			LastTerminated: &capability.Termination{Container: "app", Reason: "OOMKilled", ExitCode: 137},
		},
	}}
}

const diagnoseArgs = `{"environment":"production","namespace":"shop","kind":"Deployment","name":"api"}`

func TestDiagnoseWithoutMetricsProvider(t *testing.T) {
	ws := &fakeSource{
		workload: failingWorkload(), events: []capability.Event{{Reason: "BackOff"}},
		logs: &capability.WorkloadLogs{Logs: []capability.ContainerLog{{Pod: "api-bad", Container: "app", Lines: []string{"killed"}}}},
	}
	tl, err := DiagnoseWorkload(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := runTool(t, tl, diagnoseArgs)
	if err != nil {
		t.Fatal(err)
	}
	d := out.(*Diagnosis)
	if d.Status == nil || len(d.Events) != 1 || ws.eventLimit != 20 {
		t.Fatalf("diagnosis = %+v (event limit %d)", d, ws.eventLimit)
	}
	if o := ws.logOptions; o.Pod != "api-bad" || o.Container != "app" || !o.Previous || o.TailLines != 50 {
		t.Fatalf("logs read for the failing pod's crashed container: %+v", o)
	}
	if d.Unavailable["metrics"] != "no metrics provider configured" {
		t.Fatalf("unavailable = %v", d.Unavailable)
	}
}

func TestDiagnoseDegradesPartByPart(t *testing.T) {
	ws := &fakeSource{
		workload: failingWorkload(), eventsErr: errors.New("events forbidden"),
		logs: &capability.WorkloadLogs{},
	}
	m := &fakeMetrics{}
	tl, _ := DiagnoseWorkload(ws, m)
	_, out, err := runTool(t, tl, diagnoseArgs)
	if err != nil {
		t.Fatalf("a failed part must not fail the call: %v", err)
	}
	d := out.(*Diagnosis)
	if !strings.Contains(d.Unavailable["events"], "events forbidden") {
		t.Fatalf("unavailable = %v", d.Unavailable)
	}
	if d.Metrics["cpu"] == nil || d.Metrics["memory"] == nil {
		t.Fatalf("metrics = %+v", d.Metrics)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"status":`) {
		t.Fatalf("json = %s", b)
	}
}

func TestDiagnoseHealthyWorkloadReadsNoLogs(t *testing.T) {
	ws := &fakeSource{workload: &capability.Workload{Pods: []capability.Pod{{Name: "api-ok", Phase: "Running", Ready: true}}}}
	tl, _ := DiagnoseWorkload(ws, nil)
	if _, _, err := runTool(t, tl, diagnoseArgs); err != nil {
		t.Fatal(err)
	}
	for _, c := range ws.calls {
		if c == "logs" {
			t.Fatal("logs read for a healthy workload")
		}
	}
}

func TestDiagnoseFailsWhenTheWorkloadCannotBeRead(t *testing.T) {
	tl, _ := DiagnoseWorkload(&fakeSource{err: capability.ErrNotFound}, nil)
	if _, _, err := runTool(t, tl, diagnoseArgs); !errors.Is(err, capability.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
