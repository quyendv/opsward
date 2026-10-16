package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicVersion = "2023-06-01"
	requestTimeout   = 2 * time.Minute
	maxResponseBytes = 4 << 20
)

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
		opt.Client = &http.Client{Timeout: requestTimeout}
	}
	opt.URL = strings.TrimSuffix(opt.URL, "/")
	return &anthropic{opt}
}

func (a *anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	body := map[string]any{"model": a.opt.Model, "max_tokens": req.MaxTokens, "messages": req.Messages}
	if req.System != "" {
		body["system"] = req.System
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
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opt.URL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("model request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("x-api-key", key)
	hr.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.opt.Client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("model: %w", ctx.Err())
		}
		return nil, fmt.Errorf("model: request failed: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Type, Message string } `json:"error"`
		}
		msg := strings.TrimSpace(string(rb))
		if json.Unmarshal(rb, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Type + ": " + e.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("model: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out Response
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("model: decoding the response: %w", err)
	}
	return &out, nil
}

// unwrapURL drops the request URL a transport error carries; the error beneath says
// what went wrong.
func unwrapURL(err error) error {
	if u, ok := err.(interface{ Unwrap() error }); ok && u.Unwrap() != nil {
		return u.Unwrap()
	}
	return err
}
