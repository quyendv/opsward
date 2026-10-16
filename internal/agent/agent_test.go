package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/llm"
)

// script is a model that answers with prepared turns and records what it was sent.
type script struct {
	mu    sync.Mutex
	turns []*llm.Response
	seen  []llm.Request
	delay time.Duration
}

func (s *script) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	s.seen = append(s.seen, req)
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.turns) == 0 {
		return &llm.Response{Content: []llm.Block{{Type: "text", Text: "no more turns"}}, StopReason: "end_turn"}, nil
	}
	r := s.turns[0]
	s.turns = s.turns[1:]
	return r, nil
}

func toolUse(id, name, input string) llm.Block {
	return llm.Block{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}
}

type calls struct {
	mu   sync.Mutex
	list []string
	p    []core.Principal
}

func (c *calls) Invoke(_ context.Context, p core.Principal, tool string, args json.RawMessage) (*engine.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, tool+" "+string(args))
	c.p = append(c.p, p)
	switch tool {
	case "list_workload_logs":
		return &engine.Result{Data: map[string]any{"lines": []any{strings.Repeat("x", 40<<10)}}}, nil
	case "query_metrics":
		return nil, errors.New("denied by policy (rule default-deny)")
	}
	return &engine.Result{Data: map[string]any{"ok": true}}, nil
}

var tools = []engine.Tool{
	{Name: "list_workload_logs", Description: "logs", Risk: core.RiskRead, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "query_metrics", Description: "metrics", Risk: core.RiskRead, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "restart_workload", Description: "restart", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "propose_change", Description: "propose", Risk: core.RiskWrite, Risks: []core.Risk{core.RiskRead}, InputSchema: &jsonschema.Schema{Type: "object"}},
}

func TestInvestigateCallsReadToolsThenAnswers(t *testing.T) {
	m := &script{turns: []*llm.Response{
		{Content: []llm.Block{{Type: "text", Text: "Reading logs."}, toolUse("t1", "list_workload_logs", `{"name":"api"}`), toolUse("t2", "query_metrics", `{"query":"up"}`)}, StopReason: "tool_use"},
		{Content: []llm.Block{{Type: "text", Text: "The database password is wrong; rotate the secret."}}, StopReason: "end_turn"},
	}}
	c := &calls{}
	a := New(Options{Model: m, Invoker: c, Tools: tools})
	got, err := a.Investigate(context.Background(), core.Principal{Subject: "alertmanager"}, "Alert: api crash looping")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "The database password is wrong; rotate the secret." || got.Steps != 2 || got.Stopped != "" {
		t.Fatalf("analysis = %+v", got)
	}
	if len(c.list) != 2 || c.p[0].Adapter != Adapter || c.p[0].Subject != "alertmanager" {
		t.Fatalf("calls = %v, principals %+v", c.list, c.p)
	}
	// Only read tools are offered: nothing an agent may call can change anything.
	var offered []string
	for _, tl := range m.seen[0].Tools {
		offered = append(offered, tl.Name)
	}
	if strings.Join(offered, ",") != "list_workload_logs,query_metrics" {
		t.Fatalf("offered %v", offered)
	}
	// Results go back to the model cut to size, and a denial as an error.
	results := m.seen[1].Messages[2].Content
	if len(results) != 2 || len(results[0].Content) > 17<<10 || !strings.Contains(results[0].Content, "truncated") || !results[1].IsError || !strings.Contains(results[1].Content, "denied") {
		t.Fatalf("tool results = %+v", results)
	}
}

// A tool the model names that was not offered is refused, not run.
func TestInvestigateRefusesUnofferedTools(t *testing.T) {
	m := &script{turns: []*llm.Response{
		{Content: []llm.Block{toolUse("t1", "restart_workload", `{}`)}, StopReason: "tool_use"},
		{Content: []llm.Block{{Type: "text", Text: "ok"}}, StopReason: "end_turn"},
	}}
	c := &calls{}
	if _, err := New(Options{Model: m, Invoker: c, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, "x"); err != nil {
		t.Fatal(err)
	}
	if len(c.list) != 0 || !m.seen[1].Messages[2].Content[0].IsError {
		t.Fatalf("calls %v", c.list)
	}
}

func TestInvestigateStopsAtTheStepLimit(t *testing.T) {
	var turns []*llm.Response
	for range 10 {
		turns = append(turns, &llm.Response{Content: []llm.Block{toolUse("t", "list_workload_logs", `{}`)}, StopReason: "tool_use"})
	}
	m := &script{turns: turns}
	c := &calls{}
	got, err := New(Options{Model: m, Invoker: c, Tools: tools, MaxSteps: 3}).Investigate(context.Background(), core.Principal{Subject: "a"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.list) != 3 || got.Stopped != "step limit" {
		t.Fatalf("calls %d, analysis %+v", len(c.list), got)
	}
}

func TestInvestigateStopsAtTheTimeout(t *testing.T) {
	m := &script{delay: time.Second}
	got, err := New(Options{Model: m, Invoker: &calls{}, Tools: tools, Timeout: 50 * time.Millisecond}).Investigate(context.Background(), core.Principal{Subject: "a"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped != "timeout" {
		t.Fatalf("analysis = %+v", got)
	}
}

func TestInvestigateBoundsTheAnswer(t *testing.T) {
	m := &script{turns: []*llm.Response{{Content: []llm.Block{{Type: "text", Text: strings.Repeat("y", 5000)}}, StopReason: "end_turn"}}}
	got, _ := New(Options{Model: m, Invoker: &calls{}, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, "x")
	if len(got.Text) > 2100 {
		t.Fatalf("answer of %d bytes", len(got.Text))
	}
}
