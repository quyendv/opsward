package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicRoundTrip(t *testing.T) {
	var req map[string]any
	var raw string
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"Looking at logs."},
			{"type":"tool_use","id":"tu_1","name":"list_workload_logs","input":{"environment":"staging"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":30}}`)
	}))
	defer srv.Close()
	m := NewAnthropic(AnthropicOptions{URL: srv.URL, Model: "claude-sonnet-5-5", APIKey: func() (string, error) { return "k3y", nil }})
	resp, err := m.Complete(context.Background(), Request{
		System: "You investigate alerts.", MaxTokens: 1024,
		Tools: []Tool{{Name: "list_workload_logs", Description: "logs", InputSchema: map[string]any{"type": "object"}}},
		Messages: []Message{
			{Role: "user", Content: []Block{{Type: "text", Text: "Alert fired."}}},
			{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "tu_0", Name: "get_workload_status", Input: json.RawMessage(`{"name":"api"}`)}}},
			{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "tu_0", Content: `{"ready":0}`, IsError: true}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("x-api-key") != "k3y" || headers.Get("anthropic-version") == "" {
		t.Errorf("headers = %v", headers)
	}
	if req["model"] != "claude-sonnet-5-5" || req["system"] != "You investigate alerts." || req["max_tokens"] != float64(1024) {
		t.Errorf("request = %v", req)
	}
	for _, want := range []string{`"type":"tool_use","id":"tu_0","name":"get_workload_status","input":{"name":"api"}`, `"type":"tool_result","tool_use_id":"tu_0","content":"{\"ready\":0}","is_error":true`} {
		if !strings.Contains(raw, want) {
			t.Errorf("request misses %s: %s", want, raw)
		}
	}
	if resp.StopReason != "tool_use" || len(resp.Content) != 2 || resp.Content[1].Name != "list_workload_logs" || string(resp.Content[1].Input) != `{"environment":"staging"}` || resp.Usage.InputTokens != 120 {
		t.Fatalf("response = %+v", resp)
	}
}

// Errors carry the status and what the provider said, never the key.
func TestAnthropicErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()
	m := NewAnthropic(AnthropicOptions{URL: srv.URL, Model: "m", APIKey: func() (string, error) { return "s3cretkey", nil }})
	_, err := m.Complete(context.Background(), Request{MaxTokens: 10, Messages: []Message{{Role: "user", Content: []Block{{Type: "text", Text: "x"}}}}})
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") || strings.Contains(err.Error(), "s3cretkey") {
		t.Fatalf("err = %v", err)
	}
}
