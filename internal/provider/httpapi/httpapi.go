// Package httpapi is the HTTP client shared by query backends (Prometheus, Loki):
// timeouts, a bounded body, the configured CA, and credentials read from files.
package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Limits from docs/design/0004.
const (
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 16 << 20
)

// Options configures a Client. Credentials are file paths, read on every request so that
// rotated tokens (such as projected ServiceAccount tokens) are picked up.
type Options struct {
	URL               string
	Tenant            string // sent as X-Scope-OrgID
	BearerTokenFile   string
	BasicUsername     string
	BasicPasswordFile string
	CAFile            string
}

// Client sends GET requests to one backend.
type Client struct {
	base    *url.URL
	opt     Options
	http    *http.Client
	maxBody int64
}

// New builds a client. The CA file, when set, must be readable now.
func New(opt Options) (*Client, error) {
	base, err := url.Parse(strings.TrimSuffix(opt.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opt.CAFile != "" {
		pem, err := os.ReadFile(opt.CAFile)
		if err != nil {
			return nil, fmt.Errorf("caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("caFile %s: no PEM certificates", opt.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{base: base, opt: opt, http: &http.Client{Timeout: requestTimeout, Transport: transport}, maxBody: maxBodyBytes}, nil
}

// Get requests path (relative to the base URL) with query parameters and returns the body
// of a 2xx response. Other statuses are errors that include the start of the body.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req, path)
}

// Post sends form as an application/x-www-form-urlencoded body, which keeps long queries
// out of the URL, where proxies limit the length.
func (c *Client) Post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, path)
}

func (c *Client) do(req *http.Request, path string) ([]byte, error) {
	if c.opt.Tenant != "" {
		req.Header.Set("X-Scope-OrgID", c.opt.Tenant)
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("%s: response larger than %d bytes", path, c.maxBody)
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(body)
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(snippet))
	}
	return body, nil
}

func (c *Client) authorize(req *http.Request) error {
	switch {
	case c.opt.BearerTokenFile != "":
		token, err := readSecret(c.opt.BearerTokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	case c.opt.BasicPasswordFile != "":
		password, err := readSecret(c.opt.BasicPasswordFile)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.opt.BasicUsername, password)
	}
	return nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's configured credential file
	if err != nil {
		return "", fmt.Errorf("credential file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
