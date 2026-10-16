package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
)

type authorizerFunc func(core.Request) core.Decision

func (f authorizerFunc) Decide(r core.Request) core.Decision { return f(r) }

func allowAll(core.Request) core.Decision {
	return core.Decision{Effect: core.EffectAllow, Rule: "test-allow"}
}

type recordingAuditor struct {
	mu        sync.Mutex
	records   []audit.Record
	failOn    audit.Phase
	ctxErrors []error
}

func (a *recordingAuditor) Write(ctx context.Context, r audit.Record) (audit.Record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctxErrors = append(a.ctxErrors, ctx.Err())
	if a.failOn != "" && r.Phase == a.failOn {
		return audit.Record{}, errors.New("audit sink down")
	}
	a.records = append(a.records, r)
	return r, nil
}

func (a *recordingAuditor) phases() []audit.Phase {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Phase
	for _, r := range a.records {
		out = append(out, r.Phase)
	}
	return out
}

func (a *recordingAuditor) last() audit.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.records[len(a.records)-1]
}

// fakeTool records what the engine did with it.
type fakeTool struct {
	risk       core.Risk
	global     bool
	result     any
	runErr     error
	labels     map[string]string
	resolveErr error

	ran         bool
	resolved    bool
	ranProfile  string
	auditBefore int
	auditor     *recordingAuditor
}

func (f *fakeTool) tool() Tool {
	return Tool{
		Name: "do_thing", Risk: f.risk, Global: f.global,
		Prepare: func(raw json.RawMessage) (*Prepared, error) {
			var in struct {
				Environment string `json:"environment"`
				Namespace   string `json:"namespace"`
				Name        string `json:"name"`
				Token       string `json:"token,omitempty"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if in.Name == "" {
				return nil, errors.New("name is required")
			}
			args := map[string]any{"environment": in.Environment, "namespace": in.Namespace, "name": in.Name}
			if in.Token != "" {
				args["token"] = in.Token
			}
			return &Prepared{
				Scope: core.Scope{Environment: in.Environment, Namespace: in.Namespace, Kind: "Deployment", Name: in.Name},
				Args:  args,
				Resolve: func(context.Context) (map[string]string, error) {
					f.resolved = true
					return f.labels, f.resolveErr
				},
				Run: func(_ context.Context, profile string) (any, error) {
					f.ran, f.ranProfile = true, profile
					if f.auditor != nil {
						f.auditBefore = len(f.auditor.phases())
					}
					return f.result, f.runErr
				},
			}, nil
		},
	}
}

var alice = core.Principal{Subject: "alice@example.com", Groups: []string{"team-a"}, Adapter: "test"}

const goodArgs = `{"environment":"staging","namespace":"team-a-api","name":"api"}`

func newEngine(t *testing.T, ft *fakeTool, authz authorizerFunc, aud *recordingAuditor) *Engine {
	t.Helper()
	ft.auditor = aud
	e, err := New(Options{
		Tools:        []Tool{ft.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer:   authz,
		Auditor:      aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestInvokeAllowedRead(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, result: map[string]any{"ready": 2}}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)

	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Data.(map[string]any)["ready"] != float64(2) {
		t.Fatalf("result = %#v", got)
	}
	if ft.ranProfile != ProfileReader {
		t.Errorf("profile = %q, want reader for a read tool", ft.ranProfile)
	}
	rec := aud.last()
	if len(aud.records) != 1 || rec.Phase != audit.PhaseOutcome || rec.Outcome.Status != audit.StatusOK ||
		rec.Decision.Rule != "test-allow" || rec.Scope.Name != "api" || rec.Principal.Subject != alice.Subject {
		t.Fatalf("audit = %+v", aud.records)
	}
}

func TestInvokeRejectsBeforeRunning(t *testing.T) {
	tests := []struct {
		name    string
		p       core.Principal
		tool    string
		args    string
		authz   authorizerFunc
		wantErr error
		status  string
	}{
		{"no principal", core.Principal{}, "do_thing", goodArgs, allowAll, ErrUnidentified, audit.StatusDenied},
		{"unknown tool", alice, "rm_rf", goodArgs, allowAll, ErrUnknownTool, audit.StatusUnknownTool},
		{"invalid arguments", alice, "do_thing", `{"environment":"staging"}`, allowAll, ErrInvalidArguments, audit.StatusInvalid},
		{"unknown environment", alice, "do_thing", `{"environment":"prod","namespace":"team-a-api","name":"api"}`, allowAll, ErrInvalidArguments, audit.StatusInvalid},
		{"namespace outside environment", alice, "do_thing", `{"environment":"staging","namespace":"kube-system","name":"api"}`, allowAll, ErrInvalidArguments, audit.StatusInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft := &fakeTool{risk: core.RiskRead}
			aud := &recordingAuditor{}
			e := newEngine(t, ft, tt.authz, aud)
			_, err := e.Invoke(context.Background(), tt.p, tt.tool, json.RawMessage(tt.args))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if ft.ran || ft.resolved {
				t.Fatal("tool touched a backend for a call that was rejected during validation")
			}
			if len(aud.records) != 1 || aud.last().Outcome.Status != tt.status {
				t.Fatalf("audit = %+v, want one record with status %s", aud.records, tt.status)
			}
		})
	}
}

func TestInvokeDenied(t *testing.T) {
	for _, effect := range []core.Effect{core.EffectDeny, core.EffectApprove} {
		t.Run(string(effect), func(t *testing.T) {
			ft := &fakeTool{risk: core.RiskRead}
			aud := &recordingAuditor{}
			e := newEngine(t, ft, func(core.Request) core.Decision {
				return core.Decision{Effect: effect, Rule: "r1"}
			}, aud)
			_, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
			var denied *DeniedError
			if !errors.As(err, &denied) || denied.Decision.Rule != "r1" {
				t.Fatalf("err = %v, want DeniedError for rule r1", err)
			}
			if ft.ran {
				t.Fatal("denied tool ran")
			}
			if rec := aud.last(); rec.Outcome.Status != audit.StatusDenied || rec.Decision.Effect != effect {
				t.Fatalf("audit = %+v", rec)
			}
		})
	}
}

func TestInvokePassesLabelsToPolicy(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, labels: map[string]string{"app.kubernetes.io/part-of": "checkout"}}
	var seen core.Request
	e := newEngine(t, ft, func(r core.Request) core.Decision { seen = r; return allowAll(r) }, &recordingAuditor{})
	if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs)); err != nil {
		t.Fatal(err)
	}
	if seen.Scope.Labels["app.kubernetes.io/part-of"] != "checkout" || !seen.Scope.LabelsKnown || seen.Args["name"] != "api" {
		t.Fatalf("policy saw %+v", seen)
	}
}

func TestInvokeTellsPolicyWhenLabelsAreUnknown(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, resolveErr: errors.New("forbidden")}
	var seen core.Request
	e := newEngine(t, ft, func(r core.Request) core.Decision { seen = r; return allowAll(r) }, &recordingAuditor{})
	_, _ = e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if seen.Scope.LabelsKnown {
		t.Fatal("LabelsKnown = true after a failed lookup")
	}
}

func TestInvokeHidesLookupFailureFromUnauthorizedCaller(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, resolveErr: errors.New(`deployments "secret-project" not found`)}
	e := newEngine(t, ft, func(core.Request) core.Decision {
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	}, &recordingAuditor{})
	_, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	var denied *DeniedError
	if !errors.As(err, &denied) || strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a plain denial", err)
	}
}

func TestInvokeProfileSelection(t *testing.T) {
	for _, tt := range []struct {
		name    string
		risk    core.Risk
		profile string
		want    string
	}{
		{"read default", core.RiskRead, "", ProfileReader},
		{"write default", core.RiskWrite, "", ProfileOperator},
		{"binding profile", core.RiskRead, "team-a-reader", "team-a-reader"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ft := &fakeTool{risk: tt.risk}
			e := newEngine(t, ft, func(core.Request) core.Decision {
				return core.Decision{Effect: core.EffectAllow, Rule: "r", Profile: tt.profile}
			}, &recordingAuditor{})
			if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs)); err != nil {
				t.Fatal(err)
			}
			if ft.ranProfile != tt.want {
				t.Fatalf("profile = %q, want %q", ft.ranProfile, tt.want)
			}
		})
	}
}

func TestInvokeWriteRecordsIntentFirst(t *testing.T) {
	ft := &fakeTool{risk: core.RiskWrite}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)
	if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs)); err != nil {
		t.Fatal(err)
	}
	if got := aud.phases(); len(got) != 2 || got[0] != audit.PhaseIntent || got[1] != audit.PhaseOutcome {
		t.Fatalf("phases = %v, want [intent outcome]", got)
	}
	if ft.auditBefore != 1 {
		t.Fatalf("tool ran with %d audit records written, want the intent already written", ft.auditBefore)
	}
}

func TestInvokeRefusesWriteWhenIntentCannotBeRecorded(t *testing.T) {
	ft := &fakeTool{risk: core.RiskWrite}
	e := newEngine(t, ft, allowAll, &recordingAuditor{failOn: audit.PhaseIntent})
	_, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if !errors.Is(err, ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if ft.ran {
		t.Fatal("write ran without a recorded intent")
	}
}

func TestInvokeWithholdsResultWhenOutcomeCannotBeRecorded(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, result: "data"}
	e := newEngine(t, ft, allowAll, &recordingAuditor{failOn: audit.PhaseOutcome})
	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if !errors.Is(err, ErrAuditUnavailable) || got != nil {
		t.Fatalf("Invoke = %v, %v; want no result and ErrAuditUnavailable", got, err)
	}
}

func TestInvokeRedactsResultArgsAndErrors(t *testing.T) {
	secret := strings.Join([]string{"hunter2", "hunter2"}, "")
	ft := &fakeTool{risk: core.RiskRead, result: map[string]any{"env": "DB_PASSWORD=" + secret}}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)
	args := `{"environment":"staging","namespace":"team-a-api","name":"api","token":"` + secret + `"}`
	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got.Data)
	if strings.Contains(string(b), secret) {
		t.Fatalf("result leaked a secret: %s", b)
	}
	ab, _ := json.Marshal(aud.last())
	if strings.Contains(string(ab), secret) {
		t.Fatalf("audit leaked a secret: %s", ab)
	}

	ft.runErr = errors.New("connect: postgres://app:" + secret + "@db:5432")
	_, err = e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if err == nil {
		t.Fatal("want the tool error")
	}
	if rec := aud.last(); rec.Outcome.Status != audit.StatusError || strings.Contains(rec.Outcome.Error, secret) {
		t.Fatalf("audit error = %+v", rec.Outcome)
	}
}

func TestInvokeRedactsReturnedErrors(t *testing.T) {
	secret := strings.Join([]string{"hunter2", "hunter2"}, "")
	ft := &fakeTool{risk: core.RiskRead, runErr: fmt.Errorf("connect: postgres://app:%s@db:5432: %w", secret, errSentinel)}
	e := newEngine(t, ft, allowAll, &recordingAuditor{})
	_, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("returned error leaks the secret: %v", err)
	}
	if !errors.Is(err, errSentinel) {
		t.Fatalf("redaction lost the error chain: %v", err)
	}
}

var errSentinel = errors.New("sentinel")

func TestInvokeAuditsACancelledCall(t *testing.T) {
	ft := &fakeTool{risk: core.RiskWrite}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = e.Invoke(ctx, alice, "do_thing", json.RawMessage(goodArgs))
	if len(aud.ctxErrors) != 2 {
		t.Fatalf("audit writes = %d, want intent and outcome", len(aud.ctxErrors))
	}
	for i, err := range aud.ctxErrors {
		if err != nil {
			t.Fatalf("audit write %d got a cancelled context (%v): a client that disconnects must not erase the record", i, err)
		}
	}
}

func TestNewRejectsDuplicateTools(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead}
	_, err := New(Options{Tools: []Tool{ft.tool(), ft.tool()}, Authorizer: authorizerFunc(allowAll), Auditor: &recordingAuditor{}})
	if err == nil {
		t.Fatal("New accepted two tools with the same name")
	}
}

// Compile rejects allow on destructive tools; the engine refuses it again at run time,
// so a different Authorizer cannot reintroduce it.
func TestInvokeNeverRunsDestructiveOnPlainAllow(t *testing.T) {
	ft := &fakeTool{risk: core.RiskDestructive}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)
	_, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want DeniedError", err)
	}
	if ft.ran {
		t.Fatal("destructive tool ran on a plain allow")
	}
	if rec := aud.last(); rec.Outcome.Status != audit.StatusDenied {
		t.Fatalf("audit = %+v", rec)
	}
}

type labelBlindAuthorizer struct{ authorizerFunc }

func (labelBlindAuthorizer) NeedsLabels() bool { return false }

// When no binding looks at labels, the engine does not read them: a denied call then
// costs no backend request at all.
func TestInvokeSkipsLabelLookupWhenPolicyIgnoresLabels(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead}
	aud := &recordingAuditor{}
	ft.auditor = aud
	e, err := New(Options{
		Tools:        []Tool{ft.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer:   labelBlindAuthorizer{allowAll},
		Auditor:      aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs)); err != nil {
		t.Fatal(err)
	}
	if ft.resolved {
		t.Fatal("labels were read although policy never uses them")
	}
}

type annotatorFunc func(core.Request, any) []core.Note

func (f annotatorFunc) Annotate(r core.Request, result any) []core.Note { return f(r, result) }

func TestInvokeAttachesNotesFromTheAnnotator(t *testing.T) {
	secret := strings.Join([]string{"hunter2", "hunter2"}, "")
	ft := &fakeTool{risk: core.RiskRead, result: map[string]any{"state": "CrashLoopBackOff", "env": "DB_PASSWORD=" + secret}}
	var seen any
	ann := annotatorFunc(func(r core.Request, result any) []core.Note {
		seen = result
		if result.(map[string]any)["state"] == "CrashLoopBackOff" && r.Tool == "do_thing" {
			return []core.Note{{ID: "crashloop", Title: "Crash loop", Severity: "warning", Summary: "read previous logs"}}
		}
		return nil
	})
	aud := &recordingAuditor{}
	ft.auditor = aud
	e, err := New(Options{
		Tools:        []Tool{ft.tool()},
		Environments: []Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}},
		Authorizer:   authorizerFunc(allowAll), Auditor: aud, Annotator: ann,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != 1 || got.Notes[0].ID != "crashloop" {
		t.Fatalf("notes = %+v", got.Notes)
	}
	b, _ := json.Marshal(seen)
	if strings.Contains(string(b), secret) {
		t.Fatalf("annotator saw an unredacted result: %s", b)
	}
}

// Tools that are not bound to an environment (knowledge search) skip scope validation
// and label lookup, but are still authorized.
func TestInvokeGlobalTool(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, global: true, result: "ok"}
	var seen core.Request
	e := newEngine(t, ft, func(r core.Request) core.Decision { seen = r; return allowAll(r) }, &recordingAuditor{})
	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(`{"name":"q"}`))
	if err != nil || got.Data != "ok" {
		t.Fatalf("Invoke = %+v, %v", got, err)
	}
	if ft.resolved {
		t.Fatal("a global tool must not resolve labels")
	}
	if seen.Tool != "do_thing" {
		t.Fatal("a global tool must still be authorized")
	}

	deny := newEngine(t, &fakeTool{risk: core.RiskRead, global: true}, func(core.Request) core.Decision {
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	}, &recordingAuditor{})
	var denied *DeniedError
	if _, err := deny.Invoke(context.Background(), alice, "do_thing", json.RawMessage(`{"name":"q"}`)); !errors.As(err, &denied) {
		t.Fatalf("err = %v, want DeniedError", err)
	}
}

func TestUpdateSwapsEnvironmentsPolicyAndAnnotator(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, result: "ok"}
	e := newEngine(t, ft, func(core.Request) core.Decision {
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	}, &recordingAuditor{})
	prod := `{"environment":"production","namespace":"shop","name":"api"}`
	if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(prod)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("before Update: err = %v, want unknown environment", err)
	}
	e.Update([]Environment{{Name: "production", Namespaces: []string{"*"}}}, authorizerFunc(allowAll),
		annotatorFunc(func(core.Request, any) []core.Note { return []core.Note{{ID: "n"}} }))
	got, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(prod))
	if err != nil || len(got.Notes) != 1 {
		t.Fatalf("after Update: %+v, %v", got, err)
	}
}

func TestUpdateIsRaceFree(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, result: "ok"}
	e := newEngine(t, ft, allowAll, &recordingAuditor{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			_, _ = e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs))
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			e.Update([]Environment{{Name: "staging", Namespaces: []string{"team-a-*"}}}, authorizerFunc(allowAll), nil)
		}
	}()
	wg.Wait()
}

// A run that fails on the caller's input, such as a query outside its scope, is audited
// as invalid, not as an error of the system.
func TestInvokeRunRejectingInputIsInvalid(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead, runErr: fmt.Errorf("query: %w", core.InvalidInput("query selects outside the call's scope"))}
	aud := &recordingAuditor{}
	e := newEngine(t, ft, allowAll, aud)
	if _, err := e.Invoke(context.Background(), alice, "do_thing", json.RawMessage(goodArgs)); err == nil {
		t.Fatal("no error")
	}
	if got := aud.last().Outcome.Status; got != audit.StatusInvalid {
		t.Fatalf("status = %s, want invalid", got)
	}
}
