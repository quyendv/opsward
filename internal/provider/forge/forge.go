// Package forge implements capability.Forge on the REST APIs of GitHub, Gitea (and
// Forgejo) and GitLab. docs/design/0008.
package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/quyendv/opsward/internal/capability"
)

const (
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 8 << 20
	// maxTreePages bounds the requests one listing makes.
	maxTreePages = 20
)

// Options configures a forge.
type Options struct {
	Type  string // github, gitea or gitlab
	URL   string // API base: https://api.github.com, https://git.example.com, https://gitlab.example.com
	Token func() (string, error)
}

// New returns the forge of opt.Type.
func New(opt Options) (capability.Forge, error) {
	c := &client{base: strings.TrimSuffix(opt.URL, "/"), token: opt.Token, http: &http.Client{Timeout: requestTimeout}}
	switch opt.Type {
	case "github":
		c.auth = func(t string) (string, string) { return "Authorization", "Bearer " + t }
		return &github{c}, nil
	case "gitea":
		c.auth = func(t string) (string, string) { return "Authorization", "token " + t }
		return &gitea{c}, nil
	case "gitlab":
		c.auth = func(t string) (string, string) { return "PRIVATE-TOKEN", t }
		return &gitlab{c}, nil
	}
	return nil, fmt.Errorf("forge type %q is not supported; use github, gitea or gitlab", opt.Type)
}

// errNotFound is a 404 from the forge.
var errNotFound = errors.New("not found")

type client struct {
	base  string
	token func() (string, error)
	auth  func(token string) (header, value string)
	http  *http.Client
}

// do sends a JSON request and decodes a JSON answer into out (when not nil). Errors
// carry the status and the start of the body, never the token.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	if err := noDotSegments(path); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	tok, err := c.token()
	if err != nil {
		return fmt.Errorf("forge token: %w", err)
	}
	k, v := c.auth(tok)
	req.Header.Set(k, v)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	}
	if resp.StatusCode/100 != 2 {
		snippet := strings.TrimSpace(string(b))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func decodeContent(encoding, content string) ([]byte, error) {
	if encoding != "base64" {
		return nil, fmt.Errorf("unexpected encoding %q", encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
}

// pathEscape escapes each segment of a repository path, keeping the slashes.
func pathEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// under keeps the blobs below dir, sorted and bounded by capability.MaxListedFiles.
func under(entries []treeEntry, dir string, truncated bool) ([]string, bool) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for _, e := range entries {
		if e.Type == "blob" && strings.HasPrefix(e.Path, prefix) {
			out = append(out, e.Path)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > capability.MaxListedFiles {
		out, truncated = out[:capability.MaxListedFiles], true
	}
	return out, truncated
}

type treeEntry struct{ Path, Type string }

// noDotSegments refuses a request path, escaped or not, with a "." or ".." segment:
// clients and servers may resolve those, and leave the repository the path names.
func noDotSegments(p string) error {
	p, _, _ = strings.Cut(p, "?")
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return fmt.Errorf("request path: %w", err)
	}
	for _, s := range []string{p, decoded} {
		for _, seg := range strings.Split(s, "/") {
			if seg == "." || seg == ".." {
				return fmt.Errorf("request path %q has a dot segment", p)
			}
		}
	}
	return nil
}

// fileFrom decodes a contents API answer for path. Only a regular file is content: a
// directory answers with a list, and GitHub answers a symlink to a file with the target's
// content and path.
func fileFrom(raw json.RawMessage, path string) (content []byte, sha string, err error) {
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
		return nil, "", fmt.Errorf("%s is a directory", path)
	}
	var out struct{ Type, Path, Encoding, Content, SHA string }
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if out.Type != "file" || (out.Path != "" && out.Path != path) {
		return nil, "", fmt.Errorf("%s is not a regular file", path)
	}
	b, err := decodeContent(out.Encoding, out.Content)
	return b, out.SHA, err
}

// opened returns the request's URL, or err after deleting the branch made for it, so
// that a failed proposal leaves nothing behind.
func opened(ctx context.Context, url string, err error, deleteBranch func(context.Context) error) (string, error) {
	if err == nil {
		return url, nil
	}
	// The caller's context may be what failed; the deletion still runs, briefly.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	if derr := deleteBranch(dctx); derr != nil {
		return "", fmt.Errorf("%w; the branch it was made from could not be deleted: %w", err, derr)
	}
	return "", err
}
