package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/quyendv/opsward/internal/capability"
)

type gitlab struct{ *client }

func project(repo string) string { return "/api/v4/projects/" + url.PathEscape(repo) }

func (g *gitlab) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, bool, error) {
	var out struct{ Encoding, Content string }
	err := g.do(ctx, http.MethodGet, project(repo)+"/repository/files/"+url.PathEscape(path)+"?ref="+url.QueryEscape(ref), nil, &out)
	if errors.Is(err, errNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	b, err := decodeContent(out.Encoding, out.Content)
	return b, err == nil, err
}

// Propose creates the branch and its one commit in a single call, then a merge request.
func (g *gitlab) Propose(ctx context.Context, repo string, p capability.Proposal) (string, error) {
	actions := make([]map[string]string, 0, len(p.Files))
	for path, content := range p.Files {
		actions = append(actions, map[string]string{
			"action": "update", "file_path": path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content),
		})
	}
	if err := g.do(ctx, http.MethodPost, project(repo)+"/repository/commits", map[string]any{
		"branch": p.Branch, "start_sha": p.BaseCommit, "commit_message": p.Message, "actions": actions,
	}, nil); err != nil {
		return "", err
	}
	var mr struct {
		WebURL string `json:"web_url"`
	}
	if err := g.do(ctx, http.MethodPost, project(repo)+"/merge_requests", map[string]any{
		"source_branch": p.Branch, "target_branch": p.Base, "title": p.Title, "description": p.Body,
	}, &mr); err != nil {
		return "", err
	}
	return mr.WebURL, nil
}

func (g *gitlab) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, project(repo), nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// ListFiles pages through the recursive tree below dir.
func (g *gitlab) ListFiles(ctx context.Context, repo, ref, dir string) ([]string, bool, error) {
	const perPage = 100
	var all []treeEntry
	for page := 1; ; page++ {
		var out []treeEntry
		q := fmt.Sprintf("?path=%s&ref=%s&recursive=true&per_page=%d&page=%d", url.QueryEscape(dir), url.QueryEscape(ref), perPage, page)
		if err := g.do(ctx, http.MethodGet, project(repo)+"/repository/tree"+q, nil, &out); err != nil {
			return nil, false, err
		}
		all = append(all, out...)
		if len(out) < perPage {
			files, truncated := under(all, dir, false)
			return files, truncated, nil
		}
		if page == maxTreePages {
			files, _ := under(all, dir, true)
			return files, true, nil
		}
	}
}

func (g *gitlab) ResolveRef(ctx context.Context, repo, ref string) (string, bool, error) {
	var b struct{ Commit struct{ ID string } }
	err := g.do(ctx, http.MethodGet, project(repo)+"/repository/branches/"+url.PathEscape(ref), nil, &b)
	if err == nil {
		return b.Commit.ID, true, nil
	}
	if !errors.Is(err, errNotFound) {
		return "", false, err
	}
	var c struct{ ID string }
	if err := g.do(ctx, http.MethodGet, project(repo)+"/repository/commits/"+url.PathEscape(ref), nil, &c); err != nil {
		return "", false, fmt.Errorf("revision %s: %w", ref, err)
	}
	return c.ID, false, nil
}
