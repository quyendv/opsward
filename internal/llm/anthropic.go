package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const anthropicVersion = "2023-06-01"

// AnthropicOptions configures the Anthropic Messages API client.
type AnthropicOptions struct {
	URL    string // default https://api.anthropic.com
	Model  string
	APIKey func() (string, error) // read per request, so a rotated key is used
	Client *http.Client
}

type anthropic struct{ opt AnthropicOptions }

// NewAnthropic returns a Model on the Anthropic Messages API.
func NewAnthropic(opt AnthropicOptions) Model {
	if opt.URL == "" {
		opt.URL = "https://api.anthropic.com"
	}
	if opt.Client == nil {
		opt.Client = newClient()
	}
	opt.URL = strings.TrimSuffix(opt.URL, "/")
	return &anthropic{opt}
}

func (a *anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	body := map[string]any{"model": a.opt.Model, "max_tokens": req.MaxTokens, "messages": req.Messages}
	if req.System != "" {
		// Cached: every turn of an investigation resends it.
		body["system"] = []map[string]any{{"type": "text", "text": req.System, "cache_control": map[string]string{"type": "ephemeral"}}}
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	key, err := a.opt.APIKey()
	if err != nil {
		return nil, fmt.Errorf("model api key: %w", err)
	}
	header := http.Header{}
	header.Set("x-api-key", key)
	header.Set("anthropic-version", anthropicVersion)
	rb, err := postJSON(ctx, a.opt.Client, a.opt.URL+"/v1/messages", header, b)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      Usage             `json:"usage"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, fmt.Errorf("model: decoding the response: %w", err)
	}
	out := &Response{StopReason: raw.StopReason, Usage: raw.Usage}
	for _, c := range raw.Content {
		var b Block
		type plain Block
		if err := json.Unmarshal(c, (*plain)(&b)); err != nil {
			return nil, fmt.Errorf("model: decoding the response: %w", err)
		}
		b.Raw = slices.Clone(c)
		out.Content = append(out.Content, b)
	}
	return out, nil
}
