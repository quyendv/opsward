package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

type authorizerFunc func(core.Request) core.Decision

func (f authorizerFunc) Decide(r core.Request) core.Decision { return f(r) }

type nopAuditor struct{ principals []core.Principal }

func (a *nopAuditor) Write(_ context.Context, r audit.Record) (audit.Record, error) {
	a.principals = append(a.principals, r.Principal)
	return r, nil
}

func echoTool() engine.Tool {
	return engine.Tool{
		Name: "echo", Description: "echo the name", Risk: core.RiskRead,
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
		},
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			return &engine.Prepared{
				Scope: core.Scope{Environment: "staging", Name: in.Name},
				Args:  map[string]any{"name": in.Name},
				Run:   func(context.Context, string) (any, error) { return map[string]any{"name": in.Name}, nil },
			}, nil
		},
	}
}

func connect(t *testing.T, decide authorizerFunc, aud *nopAuditor, tools ...engine.Tool) *mcp.ClientSession {
	t.Helper()
	e, err := engine.New(engine.Options{
		Tools:        tools,
		Environments: []engine.Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   decide, Auditor: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := core.Principal{Subject: "local-admin", Groups: []string{"admin"}, Adapter: AdapterStdio}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := NewServer(e, p).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content = %+v", r.Content)
	}
	return r.Content[0].(*mcp.TextContent).Text
}

func TestListToolsCarriesRiskAnnotations(t *testing.T) {
	cs := connect(t, func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow} }, &nopAuditor{}, echoTool())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "echo" || !res.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools = %+v", res.Tools)
	}
}

func TestCallAllowedAndDenied(t *testing.T) {
	aud := &nopAuditor{}
	cs := connect(t, func(r core.Request) core.Decision {
		if r.Scope.Name == "api" {
			return core.Decision{Effect: core.EffectAllow, Rule: "ok"}
		}
		return core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
	}, aud, echoTool())

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "api"}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"name":"api"`) {
		t.Fatalf("allowed call = %+v, %v", res, err)
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "db"}})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "denied by policy (rule default-deny)") {
		t.Fatalf("denied call = %+v, %v", res, err)
	}
	if len(aud.principals) != 2 || aud.principals[0].Adapter != AdapterStdio || aud.principals[0].Subject != "local-admin" {
		t.Fatalf("audited principals = %+v", aud.principals)
	}
}

type noteAll struct{}

func (noteAll) Annotate(core.Request, any) []core.Note {
	return []core.Note{{ID: "crashloop-backoff", Title: "Crash loop", Severity: "warning", Summary: "Read the previous logs."}}
}

func TestCallSendsNotesAsASecondBlock(t *testing.T) {
	e, err := engine.New(engine.Options{
		Tools:        []engine.Tool{echoTool()},
		Environments: []engine.Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   authorizerFunc(func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow} }),
		Auditor:      &nopAuditor{}, Annotator: noteAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := NewServer(e, core.Principal{Subject: "me", Adapter: AdapterStdio}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "api"}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	notes := res.Content[1].(*mcp.TextContent).Text
	for _, want := range []string{"[warning] Crash loop", "crashloop-backoff", "Read the previous logs.", "get_knowledge"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q missing %q", notes, want)
		}
	}
}

// Results are compact JSON: indentation costs an agent tokens and tells it nothing.
func TestCallResultIsCompactJSON(t *testing.T) {
	cs := connect(t, func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow} }, &nopAuditor{}, echoTool())
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "api"}})
	if err != nil || res.IsError || text(t, res) != `{"name":"api"}` {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

// Results are read by agents, not browsers: no HTML escaping of <, > and &.
func TestCallResultIsNotHTMLEscaped(t *testing.T) {
	cs := connect(t, func(core.Request) core.Decision { return core.Decision{Effect: core.EffectAllow} }, &nopAuditor{}, echoTool())
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"name": "2 -> 3 & <x>"}})
	if err != nil || text(t, res) != `{"name":"2 -> 3 & <x>"}` {
		t.Fatalf("result = %q, %v", text(t, res), err)
	}
}
