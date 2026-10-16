package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OpenAIOptions configures a client of the OpenAI chat completions API, which many
// providers and local servers (vLLM, Ollama) also speak.
type OpenAIOptions struct {
	URL    string // the base, without /v1; default https://api.openai.com
	Model  string
	APIKey func() (string, error) // nil: no Authorization header, as local servers take
	Client *http.Client
}

type openai struct{ opt OpenAIOptions }

// NewOpenAI returns a Model on the chat completions API.
func NewOpenAI(opt OpenAIOptions) Model {
	if opt.URL == "" {
		opt.URL = "https://api.openai.com"
	}
	if opt.Client == nil {
		opt.Client = newClient()
	}
	opt.URL = strings.TrimSuffix(opt.URL, "/")
	return &openai{opt}
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Complete translates the conversation, kept in the Messages API's shape, to chat
// completions and back.
func (o *openai) Complete(ctx context.Context, req Request) (*Response, error) {
	var msgs []oaMessage
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		var text []string
		var calls []oaToolCall
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "tool_use":
				c := oaToolCall{ID: b.ID, Type: "function"}
				c.Function.Name, c.Function.Arguments = b.Name, string(b.Input)
				if c.Function.Arguments == "" {
					c.Function.Arguments = "{}"
				}
				calls = append(calls, c)
			case "tool_result":
				// Each result is its own message, before any text of the same turn.
				msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			msgs = append(msgs, oaMessage{Role: m.Role, Content: strings.Join(text, "\n"), ToolCalls: calls})
		}
	}
	body := map[string]any{"model": o.opt.Model, "max_tokens": req.MaxTokens, "messages": msgs}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
			}}
		}
		body["tools"] = tools
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if o.opt.APIKey != nil {
		key, err := o.opt.APIKey()
		if err != nil {
			return nil, fmt.Errorf("model api key: %w", err)
		}
		header.Set("Authorization", "Bearer "+key)
	}
	rb, err := postJSON(ctx, o.opt.Client, o.opt.URL+"/v1/chat/completions", header, b)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Choices []struct {
			Message      oaMessage `json:"message"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, fmt.Errorf("model: decoding the response: %w", err)
	}
	if len(raw.Choices) == 0 {
		return nil, fmt.Errorf("model: the response has no choice")
	}
	ch := raw.Choices[0]
	out := &Response{Usage: Usage{InputTokens: raw.Usage.PromptTokens, OutputTokens: raw.Usage.CompletionTokens}}
	if strings.TrimSpace(ch.Message.Content) != "" {
		out.Content = append(out.Content, Block{Type: "text", Text: ch.Message.Content})
	}
	for _, c := range ch.Message.ToolCalls {
		input := json.RawMessage(c.Function.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		out.Content = append(out.Content, Block{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: input})
	}
	switch ch.FinishReason {
	case "tool_calls":
		out.StopReason = "tool_use"
	case "length":
		out.StopReason = "max_tokens"
	case "content_filter":
		out.StopReason = "refusal"
	default:
		out.StopReason = "end_turn"
		// Some servers say stop while calling tools.
		if len(ch.Message.ToolCalls) > 0 {
			out.StopReason = "tool_use"
		}
	}
	return out, nil
}
