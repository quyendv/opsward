package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout   = 2 * time.Minute
	maxResponseBytes = 4 << 20
)

// newClient returns the HTTP client providers use. The key goes to the configured host
// only: a redirect is an error, not followed.
func newClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// postJSON posts body and returns the answer's body. An overloaded or rate-limited answer
// (429, 529, 5xx) is routine: it is retried once, after what retry-after asks. Errors
// carry the status and the provider's message, never the key or the URL.
func postJSON(ctx context.Context, c *http.Client, url string, header http.Header, body []byte) ([]byte, error) {
	rb, retryAfter, err := send(ctx, c, url, header, body)
	if retryAfter < 0 {
		return rb, err
	}
	select {
	case <-time.After(retryAfter):
	case <-ctx.Done():
		return nil, fmt.Errorf("model: %w", ctx.Err())
	}
	rb, _, err = send(ctx, c, url, header, body)
	return rb, err
}

// send makes one request. retryAfter is how long to wait before a retry worth making,
// or negative when there is none to make.
func send(ctx context.Context, c *http.Client, url string, header http.Header, body []byte) ([]byte, time.Duration, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, -1, fmt.Errorf("model request: %w", err)
	}
	hr.Header = header.Clone()
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(hr)
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
	if resp.StatusCode/100 == 2 {
		return rb, -1, nil
	}
	// Both APIs answer errors as {"error": {"type", "message"}}.
	var e struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	msg := strings.TrimSpace(string(rb))
	if json.Unmarshal(rb, &e) == nil && e.Error.Message != "" {
		msg = strings.TrimPrefix(e.Error.Type+": "+e.Error.Message, ": ")
	}
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "")
	}
	err = fmt.Errorf("model: HTTP %d: %s", resp.StatusCode, msg)
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

// unwrapURL drops the request URL a transport error carries; the error beneath says
// what went wrong.
func unwrapURL(err error) error {
	if u, ok := err.(interface{ Unwrap() error }); ok && u.Unwrap() != nil {
		return u.Unwrap()
	}
	return err
}
