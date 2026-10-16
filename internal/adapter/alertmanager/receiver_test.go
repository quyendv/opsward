package alertmanager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/notify"
)

type call struct {
	p    core.Principal
	tool string
	args map[string]any
}

type fakeEngine struct {
	mu    sync.Mutex
	calls []call
	err   error
}

func (f *fakeEngine) Invoke(_ context.Context, p core.Principal, tool string, args json.RawMessage) (*engine.Result, error) {
	var a map[string]any
	_ = json.Unmarshal(args, &a)
	f.mu.Lock()
	f.calls = append(f.calls, call{p, tool, a})
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &engine.Result{Data: map[string]any{"status": map[string]any{"name": a["name"]}}}, nil
}

type chanNotifier chan notify.Notification

// Notify fails on an ended context, as a real notifier's request would.
func (c chanNotifier) Notify(ctx context.Context, n notify.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c <- n
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newReceiver(t *testing.T, e *fakeEngine, queue int) (*Receiver, chanNotifier, *clock) {
	t.Helper()
	notes := make(chanNotifier, 16)
	clk := &clock{t: time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)}
	r := New(Options{
		Invoker: e, Notifiers: []notify.Notifier{notes}, Principal: "alertmanager",
		Mapping: func() (Mapping, []Environment) { return rules, envs },
		Dedupe:  30 * time.Minute, Workers: 1, Queue: queue, Timeout: time.Minute, Now: clk.now,
	})
	return r, notes, clk
}

const webhook = `{"version":"4","status":"firing","alerts":[
	{"status":"firing","fingerprint":"f1","startsAt":"2026-10-16T07:59:00Z",
	 "labels":{"alertname":"KubePodCrashLooping","severity":"critical","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"},
	 "annotations":{"summary":"api is crash looping"}},
	{"status":"firing","fingerprint":"f2","labels":{"alertname":"NodeDiskFull","severity":"warning","cluster":"cluster-a","node":"n1"}},
	{"status":"resolved","fingerprint":"f3","labels":{"alertname":"Old","cluster":"cluster-a","namespace":"team-a-api","deployment":"web"}}]}`

func post(t *testing.T, r *Receiver, subject, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.Handle(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alerts/alertmanager", strings.NewReader(body)), core.Principal{Subject: subject, Adapter: "http"})
	return w
}

func receive(t *testing.T, notes chanNotifier) notify.Notification {
	t.Helper()
	select {
	case n := <-notes:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	return notify.Notification{}
}

func TestReceiverInvestigatesFiringAlerts(t *testing.T) {
	e := &fakeEngine{}
	r, notes, _ := newReceiver(t, e, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	w := post(t, r, "alertmanager", webhook)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"queued":2`) {
		t.Fatalf("response %d %s", w.Code, w.Body)
	}
	got := map[string]notify.Notification{}
	for range 2 {
		n := receive(t, notes)
		got[n.Alert.Fingerprint] = n
	}
	n := got["f1"]
	if n.Workload == nil || n.Workload.Name != "api" || n.Diagnosis == nil || n.Error != "" || n.Alert.Summary != "api is crash looping" || n.Alert.Severity != "critical" {
		t.Fatalf("f1 = %+v", n)
	}
	if n := got["f2"]; n.Workload != nil || !strings.Contains(n.Error, "no namespace label") {
		t.Fatalf("f2 = %+v", n)
	}
	if len(e.calls) != 1 || e.calls[0].tool != "diagnose_workload" || e.calls[0].p.Adapter != "alertmanager" ||
		e.calls[0].p.Subject != "alertmanager" || e.calls[0].args["environment"] != "staging" || e.calls[0].args["kind"] != "Deployment" {
		t.Fatalf("calls = %+v", e.calls)
	}
}

func TestReceiverDedupesWithinTheWindow(t *testing.T) {
	e := &fakeEngine{}
	r, notes, clk := newReceiver(t, e, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	one := `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"A","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"}}]}`
	post(t, r, "alertmanager", one)
	receive(t, notes)
	if w := post(t, r, "alertmanager", one); !strings.Contains(w.Body.String(), `"duplicates":1`) {
		t.Fatalf("second post: %s", w.Body)
	}
	clk.t = clk.t.Add(31 * time.Minute)
	post(t, r, "alertmanager", one)
	receive(t, notes)
	if len(e.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(e.calls))
	}
}

// A denied or failed investigation still reaches the team, saying why.
func TestReceiverReportsAFailedInvestigation(t *testing.T) {
	e := &fakeEngine{err: errors.New("denied by policy (rule default-deny)")}
	r, notes, _ := newReceiver(t, e, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	post(t, r, "alertmanager", `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"A","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"}}]}`)
	if n := receive(t, notes); n.Workload == nil || !strings.Contains(n.Error, "denied by policy") || n.Diagnosis != nil {
		t.Fatalf("notification = %+v", n)
	}
}

func TestReceiverRefuses(t *testing.T) {
	r, _, _ := newReceiver(t, &fakeEngine{}, 1)
	if w := post(t, r, "alice@example.com", webhook); w.Code != http.StatusForbidden {
		t.Errorf("another principal: %d", w.Code)
	}
	if w := post(t, r, "alertmanager", "{"); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", w.Code)
	}
	if w := post(t, r, "alertmanager", `{"version":"3","alerts":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("version 3: %d", w.Code)
	}
	if w := post(t, r, "alertmanager", `{"version":"4","alerts":[{"status":"firing","labels":{"x":"`+strings.Repeat("a", 2<<20)+`"}}]}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", w.Code)
	}
	// Nothing runs the queue: the second alert does not fit and is dropped, not waited on.
	// Alertmanager retries a failed delivery: dropping answers 503, so dropped alerts come
	// back, and the one that was queued is then a duplicate.
	two := `{"version":"4","alerts":[{"status":"firing","fingerprint":"a","labels":{}},{"status":"firing","fingerprint":"b","labels":{}}]}`
	if w := post(t, r, "alertmanager", two); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"dropped":1`) {
		t.Errorf("full queue: %d %s", w.Code, w.Body)
	}
}

// Alerts carry what their rules put in labels and annotations: notifications are
// redacted like every result, mapping errors included.
func TestReceiverRedactsTheAlert(t *testing.T) {
	r, notes, _ := newReceiver(t, &fakeEngine{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	post(t, r, "alertmanager", `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1",
		"labels":{"alertname":"ProbeFailed","instance":"https://probe:s3cretpass@api.example.com","namespace":"https://u:p4ssw0rd@x.example.com"},
		"annotations":{"summary":"probe failed with token=AAAAAAAAAAAA"}}]}`)
	n := receive(t, notes)
	b, _ := json.Marshal(n)
	for _, secret := range []string{"s3cretpass", "AAAAAAAAAAAA", "p4ssw0rd"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("notification carries %s: %s", secret, b)
		}
	}
}

// slowEngine blocks until released or its context ends.
type slowEngine struct {
	release chan struct{}
	started chan struct{}
}

func (s *slowEngine) Invoke(ctx context.Context, _ core.Principal, _ string, _ json.RawMessage) (*engine.Result, error) {
	close(s.started)
	select {
	case <-s.release:
		return &engine.Result{Data: map[string]any{}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// An investigation that runs out of time is still reported, with the reason.
func TestReceiverReportsATimedOutInvestigation(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), started: make(chan struct{})}
	notes := make(chanNotifier, 4)
	r := New(Options{
		Invoker: e, Notifiers: []notify.Notifier{notes}, Principal: "alertmanager", Workers: 1,
		Mapping: func() (Mapping, []Environment) { return rules, envs }, Timeout: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	post(t, r, "alertmanager", `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"A","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"}}]}`)
	if n := receive(t, notes); !strings.Contains(n.Error, "deadline") {
		t.Fatalf("notification = %+v", n)
	}
}

// Stopping lets an investigation under way finish and report, and Run returns only then:
// the store it audits to closes after.
func TestReceiverDrainsOnStop(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), started: make(chan struct{})}
	notes := make(chanNotifier, 4)
	r := New(Options{
		Invoker: e, Notifiers: []notify.Notifier{notes}, Principal: "alertmanager", Workers: 1,
		Mapping: func() (Mapping, []Environment) { return rules, envs }, Timeout: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	post(t, r, "alertmanager", `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"A","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"}}]}`)
	<-e.started
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned during an investigation")
	case <-time.After(100 * time.Millisecond):
	}
	close(e.release)
	if n := receive(t, notes); n.Error != "" {
		t.Fatalf("notification = %+v", n)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A resolved alert clears its window, so it is investigated again if it fires again; a
// failed investigation does not hold the window either.
func TestReceiverDedupeFollowsResolution(t *testing.T) {
	e := &fakeEngine{}
	r, notes, _ := newReceiver(t, e, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	firing := `{"version":"4","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"A","cluster":"cluster-a","namespace":"team-a-api","deployment":"api"}}]}`
	post(t, r, "alertmanager", firing)
	receive(t, notes)
	post(t, r, "alertmanager", strings.Replace(firing, `"firing","fingerprint"`, `"resolved","fingerprint"`, 1))
	post(t, r, "alertmanager", firing)
	receive(t, notes)
	if len(e.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(e.calls))
	}
	// Without a fingerprint, the labels identify the alert.
	nofp := `{"version":"4","alerts":[{"status":"firing","labels":{"alertname":"B","cluster":"cluster-a","namespace":"team-a-api","deployment":"web"}}]}`
	post(t, r, "alertmanager", nofp)
	receive(t, notes)
	if w := post(t, r, "alertmanager", nofp); !strings.Contains(w.Body.String(), `"duplicates":1`) {
		t.Fatalf("an alert without a fingerprint is not deduplicated: %s", w.Body)
	}
}

func TestReceiverNormalizesThePrincipal(t *testing.T) {
	r := New(Options{Invoker: &fakeEngine{}, Principal: "AlertManager@Example.com", Mapping: func() (Mapping, []Environment) { return rules, envs }})
	if w := post(t, r, "alertmanager@example.com", `{"version":"4","alerts":[]}`); w.Code != http.StatusAccepted {
		t.Fatalf("code %d", w.Code)
	}
}

type recordedRefusal struct {
	p      core.Principal
	reason string
}

// A delivery from another principal is a denial, and denials are audited.
func TestReceiverAuditsRefusals(t *testing.T) {
	var got []recordedRefusal
	r := New(Options{
		Invoker: &fakeEngine{}, Principal: "alertmanager", Mapping: func() (Mapping, []Environment) { return rules, envs },
		Refused: func(_ context.Context, p core.Principal, reason string) {
			got = append(got, recordedRefusal{p, reason})
		},
	})
	post(t, r, "alice@example.com", webhook)
	if len(got) != 1 || got[0].p.Subject != "alice@example.com" || got[0].p.Adapter != Adapter || !strings.Contains(got[0].reason, "alerts.principal") {
		t.Fatalf("refusals = %+v", got)
	}
}
