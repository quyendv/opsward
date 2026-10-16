package tool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// Issue #13: the metrics part has one deadline and stops at its first error, and log
// errors are kept per pod.
func TestDiagnoseBoundsMetricsAndKeepsEveryLogError(t *testing.T) {
	w := failingWorkload()
	w.Pods = append(w.Pods, capability.Pod{Name: "api-bad2", Phase: "Running", Waiting: "CrashLoopBackOff"})
	ws := &fakeSource{workload: w, logsErr: errors.New("logs forbidden")}
	m := &fakeMetrics{fail: &struct {
		match string
		err   error
	}{"container_cpu_usage_seconds_total", errors.New("backend timeout")}}
	tl, _ := DiagnoseWorkload(ws, m)
	_, out, err := runTool(t, tl, diagnoseArgs)
	if err != nil {
		t.Fatal(err)
	}
	d := out.(*Diagnosis)
	if len(m.queries) != 1 || !m.deadlines[0] {
		t.Fatalf("metrics queries = %d (deadline %v); want one, under a deadline, then stop", len(m.queries), m.deadlines)
	}
	if !strings.Contains(d.Unavailable["metrics"], "backend timeout") {
		t.Fatalf("unavailable = %v", d.Unavailable)
	}
	if d.Unavailable["logs.api-bad"] == "" || d.Unavailable["logs.api-bad2"] == "" {
		t.Fatalf("log errors per pod: %v", d.Unavailable)
	}
}

// A pod that restarted long ago and is healthy now is not failing: its old crash logs
// would read as current.
func TestDiagnoseSkipsOldRestarts(t *testing.T) {
	old := &capability.Workload{Kind: "Deployment", Name: "api", Pods: []capability.Pod{{
		Name: "api-a", Phase: "Running", Ready: true, Restarts: 5,
		LastTerminated: &capability.Termination{Container: "app", Reason: "Error", ExitCode: 100, FinishedAt: time.Now().Add(-35 * 24 * time.Hour)},
	}}}
	ws := &fakeSource{workload: old, logs: &capability.WorkloadLogs{}}
	tl, _ := DiagnoseWorkload(ws, nil)
	if _, _, err := runTool(t, tl, diagnoseArgs); err != nil {
		t.Fatal(err)
	}
	for _, c := range ws.calls {
		if c == "logs" {
			t.Fatal("read logs of a crash 35 days ago on a healthy pod")
		}
	}

	recent := &capability.Workload{Kind: "Deployment", Name: "api", Pods: []capability.Pod{{
		Name: "api-a", Phase: "Running", Ready: true, Restarts: 1,
		LastTerminated: &capability.Termination{Container: "app", Reason: "OOMKilled", ExitCode: 137, FinishedAt: time.Now().Add(-10 * time.Minute)},
	}}}
	ws = &fakeSource{workload: recent, logs: &capability.WorkloadLogs{}}
	tl, _ = DiagnoseWorkload(ws, nil)
	if _, _, err := runTool(t, tl, diagnoseArgs); err != nil {
		t.Fatal(err)
	}
	if o := ws.logOptions; o.Pod != "api-a" || !o.Previous {
		t.Fatalf("a crash 10 minutes ago: log options %+v", o)
	}
}
