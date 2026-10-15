package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	mu      sync.Mutex
	records []audit.Record
	failOn  audit.Phase
}

func (a *recordingAuditor) Write(_ context.Context, r audit.Record) (audit.Record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
		Name: "do_thing", Risk: f.risk,
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
	if got.(map[string]any)["ready"] != float64(2) {
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
	if seen.Scope.Labels["app.kubernetes.io/part-of"] != "checkout" || seen.Args["name"] != "api" {
		t.Fatalf("policy saw %+v", seen)
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
	b, _ := json.Marshal(got)
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

func TestNewRejectsDuplicateTools(t *testing.T) {
	ft := &fakeTool{risk: core.RiskRead}
	_, err := New(Options{Tools: []Tool{ft.tool(), ft.tool()}, Authorizer: authorizerFunc(allowAll), Auditor: &recordingAuditor{}})
	if err == nil {
		t.Fatal("New accepted two tools with the same name")
	}
}
