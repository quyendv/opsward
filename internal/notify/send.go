package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const sendTimeout = 15 * time.Second

// Webhook posts each notification as JSON, with a bearer token if Token is set.
type Webhook struct {
	URL    string
	Token  func() (string, error) // nil: no Authorization header
	Client *http.Client
}

// Notify implements Notifier.
func (h *Webhook) Notify(ctx context.Context, n Notification) error {
	header := http.Header{}
	if h.Token != nil {
		t, err := h.Token()
		if err != nil {
			return fmt.Errorf("webhook token: %w", err)
		}
		header.Set("Authorization", "Bearer "+t)
	}
	return post(ctx, h.Client, h.URL, header, n, "webhook")
}

// Slack posts a short text to an incoming webhook. The URL is the credential, so it is
// read from a file, per message.
type Slack struct {
	URL    func() (string, error)
	Client *http.Client
}

// Notify implements Notifier.
func (s *Slack) Notify(ctx context.Context, n Notification) error {
	u, err := s.URL()
	if err != nil {
		return fmt.Errorf("slack url: %w", err)
	}
	return post(ctx, s.Client, u, http.Header{}, map[string]string{"text": Text(n)}, "slack")
}

// post sends v as JSON. Its errors name what failed and the status, never the URL.
func post(ctx context.Context, c *http.Client, url string, header http.Header, v any, what string) error {
	if c == nil {
		c = &http.Client{Timeout: sendTimeout}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%s: invalid url", what)
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		// The error of a request names its URL: report its kind only.
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		return fmt.Errorf("%s: request failed", what)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}
	return nil
}
