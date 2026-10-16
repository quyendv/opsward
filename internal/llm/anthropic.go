package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
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
		// The key goes to the configured host only: a redirect is an error, not followed.
		opt.Client = &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	out, retryAfter, err := a.send(ctx, key, b)
	if retryAfter < 0 {
		return out, err
	}
	// Overloaded or rate limited: routine, so one retry, after what the API asked.
	select {
	case <-time.After(retryAfter):
	case <-ctx.Done():
		return nil, fmt.Errorf("model: %w", ctx.Err())
	}
	out, _, err = a.send(ctx, key, b)
	return out, err
}

// send makes one request. retryAfter is how long to wait before a retry worth making,
// or negative when there is none to make.
func (a *anthropic) send(ctx context.Context, key string, b []byte) (*Response, time.Duration, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opt.URL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, -1, fmt.Errorf("model request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("x-api-key", key)
	hr.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.opt.Client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, fmt.Errorf("model: %w", ctx.Err())
		}
		return nil, -1, fmt.Errorf("model: request failed: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, -1, fmt.Errorf("model: %w", err)
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
			msg = strings.ToValidUTF8(msg[:300], "")
		}
		err := fmt.Errorf("model: HTTP %d: %s", resp.StatusCode, msg)
		switch resp.StatusCode {
		case http.StatusTooManyRequests, 529, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			wait := 2 * time.Second
			if s, perr := strconv.Atoi(resp.Header.Get("retry-after")); perr == nil && s >= 0 {
				wait = min(time.Duration(s)*time.Second, 30*time.Second)
			}
			return nil, wait, err
		}
		return nil, -1, err
	}
	var raw struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      Usage             `json:"usage"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, -1, fmt.Errorf("model: decoding the response: %w", err)
	}
	out := &Response{StopReason: raw.StopReason, Usage: raw.Usage}
	for _, c := range raw.Content {
		var b Block
		type plain Block
		if err := json.Unmarshal(c, (*plain)(&b)); err != nil {
			return nil, -1, fmt.Errorf("model: decoding the response: %w", err)
		}
		b.Raw = slices.Clone(c)
		out.Content = append(out.Content, b)
	}
	return out, -1, nil
}

// unwrapURL drops the request URL a transport error carries; the error beneath says
// what went wrong.
func unwrapURL(err error) error {
	if u, ok := err.(interface{ Unwrap() error }); ok && u.Unwrap() != nil {
		return u.Unwrap()
	}
	return err
}
