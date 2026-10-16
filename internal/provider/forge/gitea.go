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

// gitea serves Gitea and Forgejo, whose APIs are the same for these calls.
type gitea struct{ *client }

func (g *gitea) file(ctx context.Context, repo, ref, path string) (content []byte, sha string, found bool, err error) {
	var out struct{ Type, Encoding, Content, SHA string }
	err = g.do(ctx, http.MethodGet, "/api/v1/repos/"+repo+"/contents/"+pathEscape(path)+"?ref="+url.QueryEscape(ref), nil, &out)
	if errors.Is(err, errNotFound) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	b, err := decodeContent(out.Encoding, out.Content)
	return b, out.SHA, err == nil, err
}

func (g *gitea) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, bool, error) {
	b, _, found, err := g.file(ctx, repo, ref, path)
	return b, found, err
}

// Propose changes several files in one commit on a new branch (the change-files API),
// then opens a pull request.
func (g *gitea) Propose(ctx context.Context, repo string, p capability.Proposal) (string, error) {
	files := make([]map[string]string, 0, len(p.Files))
	for path, content := range p.Files {
		_, sha, found, err := g.file(ctx, repo, p.Base, path)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errors.New(path + " does not exist on " + p.Base)
		}
		files = append(files, map[string]string{
			"operation": "update", "path": path, "sha": sha, "content": base64.StdEncoding.EncodeToString(content),
		})
	}
	r := "/api/v1/repos/" + repo
	if err := g.do(ctx, http.MethodPost, r+"/contents", map[string]any{
		"branch": p.Base, "new_branch": p.Branch, "message": p.Message, "files": files,
	}, nil); err != nil {
		return "", err
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(ctx, http.MethodPost, r+"/pulls", map[string]any{"title": p.Title, "head": p.Branch, "base": p.Base, "body": p.Body}, &pr); err != nil {
		return "", err
	}
	return pr.HTMLURL, nil
}

func (g *gitea) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, "/api/v1/repos/"+repo, nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// ListFiles pages through the recursive tree.
func (g *gitea) ListFiles(ctx context.Context, repo, ref, dir string) ([]string, bool, error) {
	var all []treeEntry
	for page := 1; ; page++ {
		var out struct {
			Tree      []treeEntry
			Truncated bool
		}
		q := fmt.Sprintf("?recursive=true&per_page=1000&page=%d", page)
		if err := g.do(ctx, http.MethodGet, "/api/v1/repos/"+repo+"/git/trees/"+pathEscape(ref)+q, nil, &out); err != nil {
			return nil, false, err
		}
		all = append(all, out.Tree...)
		if !out.Truncated || len(out.Tree) == 0 {
			files, truncated := under(all, dir, false)
			return files, truncated, nil
		}
		if page == maxTreePages {
			files, _ := under(all, dir, true)
			return files, true, nil
		}
	}
}
