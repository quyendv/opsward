package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/engine"
)

// fakeSource returns a canned workload and records which profiles were used.
type fakeSource struct {
	workload *capability.Workload
	err      error
	profiles []string
	calls    []string

	events     []capability.Event
	logs       *capability.WorkloadLogs
	logOptions capability.LogOptions
	eventsErr  error
	eventLimit int
}

func (f *fakeSource) Workloads(_, profile string) (capability.Workloads, error) {
	f.profiles = append(f.profiles, profile)
	return f, nil
}

func (f *fakeSource) GetWorkload(context.Context, capability.WorkloadRef) (*capability.Workload, error) {
	f.calls = append(f.calls, "get")
	return f.workload, f.err
}

func (f *fakeSource) WorkloadEvents(_ context.Context, _ capability.WorkloadRef, limit int) ([]capability.Event, error) {
	f.calls = append(f.calls, "events")
	f.eventLimit = limit
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	return f.events, f.err
}

func (f *fakeSource) WorkloadLogs(_ context.Context, _ capability.WorkloadRef, opt capability.LogOptions) (*capability.WorkloadLogs, error) {
	f.calls = append(f.calls, "logs")
	f.logOptions = opt
	return f.logs, f.err
}

func (f *fakeSource) WorkloadLabels(context.Context, capability.WorkloadRef) (map[string]string, error) {
	f.calls = append(f.calls, "labels")
	if f.err != nil {
		return nil, f.err
	}
	return f.workload.Labels, nil
}

const validArgs = `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"}`

func prepare(t *testing.T, src capability.WorkloadSource, args string) (*engine.Prepared, error) {
	t.Helper()
	tl, err := WorkloadStatus(src)
	if err != nil {
		t.Fatal(err)
	}
	return tl.Prepare(json.RawMessage(args))
}

func TestWorkloadStatusSchema(t *testing.T) {
	tl, err := WorkloadStatus(&fakeSource{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tl.InputSchema)
	for _, want := range []string{`"required":["environment","namespace","kind","name"]`, `"enum":["Deployment","StatefulSet","DaemonSet"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("schema %s missing %s", b, want)
		}
	}
	if len(tl.Args) != 4 {
		t.Errorf("Args = %v", tl.Args)
	}
}

func TestWorkloadStatusPrepareValidates(t *testing.T) {
	for name, args := range map[string]string{
		"missing name":   `{"environment":"staging","namespace":"shop","kind":"Deployment"}`,
		"empty name":     `{"environment":"staging","namespace":"shop","kind":"Deployment","name":""}`,
		"bad kind":       `{"environment":"staging","namespace":"shop","kind":"Pod","name":"api"}`,
		"extra argument": `{"environment":"staging","namespace":"shop","kind":"Deployment","name":"api","verb":"delete"}`,
		"not an object":  `["api"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepare(t, &fakeSource{}, args); err == nil {
				t.Fatalf("Prepare(%s) succeeded", args)
			}
		})
	}
}

// Before authorization only the labels are read, with the reader profile; the
// workload itself is read after authorization with the call's own profile.
func TestWorkloadStatusResolveReadsLabelsOnly(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{Name: "api", Labels: map[string]string{"app.kubernetes.io/part-of": "checkout"}}}
	p, err := prepare(t, src, validArgs)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Environment != "staging" || p.Scope.Namespace != "shop" || p.Scope.Kind != "Deployment" || p.Scope.Name != "api" {
		t.Fatalf("scope = %+v", p.Scope)
	}
	labels, err := p.Resolve(context.Background())
	if err != nil || labels["app.kubernetes.io/part-of"] != "checkout" {
		t.Fatalf("Resolve = %v, %v", labels, err)
	}
	if _, err := p.Run(context.Background(), "team-a-reader"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(src.calls, ",") != "labels,get" || strings.Join(src.profiles, ",") != "reader,team-a-reader" {
		t.Fatalf("calls = %v with profiles %v; want labels as reader, then get as team-a-reader", src.calls, src.profiles)
	}
}

func TestWorkloadStatusRereadsWithANarrowerProfile(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{Name: "api"}}
	p, err := prepare(t, src, validArgs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), "team-a-reader"); err != nil {
		t.Fatal(err)
	}
	if len(src.profiles) != 2 || src.profiles[1] != "team-a-reader" {
		t.Fatalf("profiles used = %v, want a second read with team-a-reader", src.profiles)
	}
}

func TestWorkloadStatusRunReportsNotFound(t *testing.T) {
	src := &fakeSource{err: capability.ErrNotFound}
	p, err := prepare(t, src, validArgs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve should report the lookup error to the engine")
	}
	if _, err := p.Run(context.Background(), engine.ProfileReader); !errors.Is(err, capability.ErrNotFound) {
		t.Fatalf("Run err = %v, want ErrNotFound", err)
	}
}

// Labels are read for policy and, per docs/design/0002, not returned to the caller.
func TestWorkloadStatusDoesNotReturnLabels(t *testing.T) {
	src := &fakeSource{workload: &capability.Workload{Name: "api", Labels: map[string]string{"team": "a"}}}
	p, err := prepare(t, src, validArgs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), engine.ProfileReader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "labels") || strings.Contains(string(b), `"team"`) {
		t.Fatalf("result exposes labels: %s", b)
	}
}
