package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := valid(req); err != nil {
		return nil, err
	}
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

// valid checks a request as the Messages API does: roles alternate from user, every
// tool_use is answered by a tool_result in the next message, text is never empty.
func valid(req llm.Request) error {
	for i, m := range req.Messages {
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if m.Role != want {
			return fmt.Errorf("message %d is %s, want %s", i, m.Role, want)
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) == "" {
				return fmt.Errorf("message %d has an empty text block", i)
			}
			if b.Type != "tool_use" {
				continue
			}
			if i+1 == len(req.Messages) {
				return fmt.Errorf("tool_use %s is the last message", b.ID)
			}
			answered := false
			for _, r := range req.Messages[i+1].Content {
				answered = answered || (r.Type == "tool_result" && r.ToolUseID == b.ID)
			}
			if !answered {
				return fmt.Errorf("tool_use %s has no tool_result", b.ID)
			}
		}
	}
	return nil
}

func toolUse(id, name, input string) llm.Block {
	return llm.Block{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}
}

type calls struct {
	mu         sync.Mutex
	list       []string
	p          []core.Principal
	unattended []bool
}

func (c *calls) Invoke(ctx context.Context, p core.Principal, tool string, args json.RawMessage) (*engine.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, tool+" "+string(args))
	c.p = append(c.p, p)
	c.unattended = append(c.unattended, core.Unattended(ctx))
	switch tool {
	case "list_workload_logs":
		return &engine.Result{Data: map[string]any{"lines": []any{strings.Repeat("x", 40<<10)}}}, nil
	case "query_metrics":
		return nil, errors.New("denied by policy (rule default-deny)")
	case "get_workload_status":
		return &engine.Result{Data: map[string]any{"ok": true}, Notes: []core.Note{{ID: "pack/crashloop", Summary: "check the last termination"}}}, nil
	}
	return &engine.Result{Data: map[string]any{"ok": true}}, nil
}

func scoped() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{
		"environment": {Type: "string"}, "namespace": {Type: "string"}, "name": {Type: "string"},
	}}
}

var tools = []engine.Tool{
	{Name: "list_workload_logs", Description: "logs", Risk: core.RiskRead, InputSchema: scoped()},
	{Name: "query_metrics", Description: "metrics", Risk: core.RiskRead, InputSchema: scoped()},
	{Name: "get_workload_status", Description: "status", Risk: core.RiskRead, InputSchema: scoped()},
	{Name: "search_knowledge", Description: "knowledge", Risk: core.RiskRead, Global: true, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "list_approvals", Description: "approvals", Risk: core.RiskRead, Global: true, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "restart_workload", Description: "restart", Risk: core.RiskWrite, InputSchema: &jsonschema.Schema{Type: "object"}},
	{Name: "propose_change", Description: "propose", Risk: core.RiskWrite, Risks: []core.Risk{core.RiskRead}, InputSchema: &jsonschema.Schema{Type: "object"}},
}

var target = Scope{Environment: "staging", Namespace: "team-a-api"}

func TestInvestigateCallsReadToolsThenAnswers(t *testing.T) {
	m := &script{turns: []*llm.Response{
		{Content: []llm.Block{{Type: "text", Text: "Reading logs."}, toolUse("t1", "list_workload_logs", `{"name":"api"}`), toolUse("t2", "query_metrics", `{"query":"up"}`)}, StopReason: "tool_use"},
		{Content: []llm.Block{{Type: "text", Text: "The database password is wrong; rotate the secret."}}, StopReason: "end_turn"},
	}}
	c := &calls{}
	a := New(Options{Model: m, Invoker: c, Tools: tools})
	got, err := a.Investigate(context.Background(), core.Principal{Subject: "alertmanager"}, target, "Alert: api crash looping")
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
	if strings.Join(offered, ",") != "list_workload_logs,query_metrics,get_workload_status,search_knowledge" {
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
	if _, err := New(Options{Model: m, Invoker: c, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x"); err != nil {
		t.Fatal(err)
	}
	if len(c.list) != 0 || !m.seen[1].Messages[2].Content[0].IsError {
		t.Fatalf("calls %v", c.list)
	}
}

func TestInvestigateStopsAtTheStepLimit(t *testing.T) {
	var turns []*llm.Response
	for i := range 10 {
		turns = append(turns, &llm.Response{Content: []llm.Block{toolUse(fmt.Sprintf("t%d", i), "list_workload_logs", `{}`)}, StopReason: "tool_use"})
	}
	m := &script{turns: turns}
	c := &calls{}
	got, err := New(Options{Model: m, Invoker: c, Tools: tools, MaxSteps: 3}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.list) != 3 || got.Stopped != "step limit" {
		t.Fatalf("calls %d, analysis %+v", len(c.list), got)
	}
}

func TestInvestigateStopsAtTheTimeout(t *testing.T) {
	m := &script{delay: time.Second}
	got, err := New(Options{Model: m, Invoker: &calls{}, Tools: tools, Timeout: 50 * time.Millisecond}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped != "timeout" {
		t.Fatalf("analysis = %+v", got)
	}
}

func TestInvestigateBoundsTheAnswer(t *testing.T) {
	m := &script{turns: []*llm.Response{{Content: []llm.Block{{Type: "text", Text: strings.Repeat("y", 5000)}}, StopReason: "end_turn"}}}
	got, _ := New(Options{Model: m, Invoker: &calls{}, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x")
	if len(got.Text) > 2100 {
		t.Fatalf("answer of %d bytes", len(got.Text))
	}
}

// Whatever the model asks for, a scoped call stays in the alert's environment and
// namespace, carries no approval, and is marked as nobody waiting on it.
func TestInvestigatePinsCallsToTheAlert(t *testing.T) {
	m := &script{turns: []*llm.Response{
		{Content: []llm.Block{
			toolUse("t1", "list_workload_logs", `{"environment":"production","namespace":"team-b","name":"db","approval":"01abc"}`),
			toolUse("t2", "query_metrics", `{"query":"up"}`),
			toolUse("t3", "search_knowledge", `{"query":"crash"}`),
		}, StopReason: "tool_use"},
		{Content: []llm.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"},
	}}
	c := &calls{}
	if _, err := New(Options{Model: m, Invoker: c, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`list_workload_logs {"environment":"staging","name":"db","namespace":"team-a-api"}`,
		`query_metrics {"environment":"staging","namespace":"team-a-api","query":"up"}`,
		`search_knowledge {"query":"crash"}`,
	}
	if strings.Join(c.list, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(c.list, "\n"))
	}
	for i, u := range c.unattended {
		if !u {
			t.Fatalf("call %d is not marked unattended", i)
		}
	}
}

// Results share one budget: past it, calls are not run, and the model is told why.
func TestInvestigateBoundsTheResults(t *testing.T) {
	var turns []*llm.Response
	for i := range 8 {
		turns = append(turns, &llm.Response{Content: []llm.Block{toolUse(fmt.Sprintf("t%d", i), "list_workload_logs", `{}`)}, StopReason: "tool_use"})
	}
	m := &script{turns: turns}
	c := &calls{}
	if _, err := New(Options{Model: m, Invoker: c, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x"); err != nil {
		t.Fatal(err)
	}
	// Each logs result is cut to 16 KiB; the budget of 96 KiB holds six.
	if len(c.list) != 6 {
		t.Fatalf("calls = %d, want 6", len(c.list))
	}
}

// A turn the model could not finish is not an answer.
func TestInvestigateReportsHowTheModelStopped(t *testing.T) {
	m := &script{turns: []*llm.Response{{Content: []llm.Block{{Type: "text", Text: "The cause is"}}, StopReason: "max_tokens"}}}
	got, _ := New(Options{Model: m, Invoker: &calls{}, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x")
	if got.Stopped != "max_tokens" || got.Text != "" {
		t.Fatalf("analysis = %+v", got)
	}
}

// Knowledge notes on a result reach the model with it.
func TestInvestigatePassesNotes(t *testing.T) {
	m := &script{turns: []*llm.Response{
		{Content: []llm.Block{toolUse("t1", "get_workload_status", `{"name":"api"}`)}, StopReason: "tool_use"},
		{Content: []llm.Block{{Type: "text", Text: "ok"}}, StopReason: "end_turn"},
	}}
	if _, err := New(Options{Model: m, Invoker: &calls{}, Tools: tools}).Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x"); err != nil {
		t.Fatal(err)
	}
	if r := m.seen[1].Messages[2].Content[0].Content; !strings.Contains(r, "check the last termination") {
		t.Fatalf("result = %s", r)
	}
}

// Tools the principal may not call are not offered: the model would spend steps on
// denials.
func TestInvestigateOffersOnlyAllowedTools(t *testing.T) {
	m := &script{turns: []*llm.Response{{Content: []llm.Block{{Type: "text", Text: "ok"}}, StopReason: "end_turn"}}}
	a := New(Options{Model: m, Invoker: &calls{}, Tools: tools, Allowed: func(_ core.Principal, tool string, s core.Scope) bool {
		return tool == "get_workload_status" && s.Environment == "staging"
	}})
	if _, err := a.Investigate(context.Background(), core.Principal{Subject: "a"}, target, "x"); err != nil {
		t.Fatal(err)
	}
	if len(m.seen[0].Tools) != 1 || m.seen[0].Tools[0].Name != "get_workload_status" {
		t.Fatalf("offered %+v", m.seen[0].Tools)
	}
}
