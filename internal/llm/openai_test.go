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

// The conversation, kept in Messages API shape, is sent as chat completions: tool calls
// on the assistant message, each result as a tool message.
func TestOpenAIRoundTrip(t *testing.T) {
	var req struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Checking logs.",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_workload_logs","arguments":"{\"name\":\"api\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":7}}`)
	}))
	defer srv.Close()
	m := NewOpenAI(OpenAIOptions{URL: srv.URL, Model: "qwen2.5:3b", APIKey: func() (string, error) { return "k", nil }})
	resp, err := m.Complete(context.Background(), Request{
		System: "sys", MaxTokens: 100,
		Tools: []Tool{{Name: "list_workload_logs", Description: "logs", InputSchema: map[string]any{"type": "object"}}},
		Messages: []Message{
			{Role: "user", Content: []Block{{Type: "text", Text: "Alert"}}},
			{Role: "assistant", Content: []Block{{Type: "text", Text: "Looking."}, {Type: "tool_use", ID: "c0", Name: "get_workload_status", Input: json.RawMessage(`{"name":"api"}`)}}},
			{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "c0", Content: `{"ready":0}`}, {Type: "text", Text: "Answer now."}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer k" || req.Model != "qwen2.5:3b" || len(req.Tools) != 1 || req.Tools[0]["type"] != "function" {
		t.Fatalf("auth %q, request %+v", auth, req)
	}
	roles := []string{}
	for _, m := range req.Messages {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %v", roles)
	}
	b, _ := json.Marshal(req.Messages)
	for _, want := range []string{`"tool_call_id":"c0"`, `"arguments":"{\"name\":\"api\"}"`, `"content":"Answer now."`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("messages miss %s: %s", want, b)
		}
	}
	if resp.StopReason != "tool_use" || len(resp.Content) != 2 || resp.Content[1].ID != "call_1" || string(resp.Content[1].Input) != `{"name":"api"}` || resp.Usage.InputTokens != 50 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestOpenAIStopReasons(t *testing.T) {
	for finish, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"`+finish+`"}],"usage":{}}`)
		}))
		m := NewOpenAI(OpenAIOptions{URL: srv.URL, Model: "m"})
		resp, err := m.Complete(context.Background(), Request{MaxTokens: 1, Messages: []Message{{Role: "user", Content: []Block{{Type: "text", Text: "x"}}}}})
		srv.Close()
		if err != nil || resp.StopReason != want {
			t.Errorf("%s: %+v, %v", finish, resp, err)
		}
	}
}
