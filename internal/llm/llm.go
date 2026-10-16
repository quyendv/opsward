// Package llm talks to model providers for agent mode (docs/design/0013): one request,
// one response, with tool use. It speaks each provider's HTTP API directly.
package llm

import (
	"context"
	"encoding/json"
)

// Tool is a tool the model may call.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// Block is one piece of a message: text, a tool call (tool_use) or its result
// (tool_result).
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Message is a turn of the conversation, by user or assistant.
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// Request is one call to the model.
type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Usage counts tokens.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the model's turn. StopReason is end_turn, tool_use or max_tokens.
type Response struct {
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Model completes requests.
type Model interface {
	Complete(ctx context.Context, req Request) (*Response, error)
}
