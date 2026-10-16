package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/quyendv/opsward/internal/capability"
)

type github struct{ *client }

func (g *github) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, bool, error) {
	var raw json.RawMessage
	err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+pathEscape(path)+"?ref="+url.QueryEscape(ref), nil, &raw)
	if errors.Is(err, errNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	b, _, err := fileFrom(raw, path)
	return b, err == nil, err
}

// Propose writes one commit through the Git data API: a tree on the base's tree, a
// commit on the base, and a branch at the commit.
func (g *github) Propose(ctx context.Context, repo string, p capability.Proposal) (string, error) {
	r := "/repos/" + repo
	var base struct{ Tree struct{ SHA string } }
	if err := g.do(ctx, http.MethodGet, r+"/git/commits/"+p.BaseCommit, nil, &base); err != nil {
		return "", err
	}
	// Each entry keeps its mode: a file written with the wrong one loses its executable
	// bit, or a symlink becomes a file.
	var old struct {
		Tree []struct{ Path, Mode string }
	}
	if err := g.do(ctx, http.MethodGet, r+"/git/trees/"+base.Tree.SHA+"?recursive=1", nil, &old); err != nil {
		return "", err
	}
	modes := map[string]string{}
	for _, e := range old.Tree {
		modes[e.Path] = e.Mode
	}
	entries := make([]map[string]string, 0, len(p.Files))
	for path, content := range p.Files {
		mode := modes[path]
		switch mode {
		case "100644", "100755":
		case "":
			mode = "100644" // not in a truncated listing
		default:
			return "", fmt.Errorf("%s is not a regular file", path)
		}
		entries = append(entries, map[string]string{"path": path, "mode": mode, "type": "blob", "content": string(content)})
	}
	var tree, commit struct{ SHA string }
	if err := g.do(ctx, http.MethodPost, r+"/git/trees", map[string]any{"base_tree": base.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	if err := g.do(ctx, http.MethodPost, r+"/git/commits", map[string]any{"message": p.Message, "tree": tree.SHA, "parents": []string{p.BaseCommit}}, &commit); err != nil {
		return "", err
	}
	if err := g.do(ctx, http.MethodPost, r+"/git/refs", map[string]any{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}, nil); err != nil {
		return "", err
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	err := g.do(ctx, http.MethodPost, r+"/pulls", map[string]any{"title": p.Title, "head": p.Branch, "base": p.Base, "body": p.Body}, &pr)
	return opened(ctx, pr.HTMLURL, err, func(ctx context.Context) error {
		return g.do(ctx, http.MethodDelete, r+"/git/refs/heads/"+pathEscape(p.Branch), nil, nil)
	})
}

func (g *github) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repo, nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// ListFiles reads the whole tree in one request; GitHub truncates very large trees.
func (g *github) ListFiles(ctx context.Context, repo, ref, dir string) ([]string, bool, error) {
	var out struct {
		Tree      []treeEntry
		Truncated bool
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/git/trees/"+pathEscape(ref)+"?recursive=1", nil, &out); err != nil {
		return nil, false, err
	}
	files, truncated := under(out.Tree, dir, out.Truncated)
	return files, truncated, nil
}

func (g *github) ResolveRef(ctx context.Context, repo, ref string) (string, bool, error) {
	var b struct{ Commit struct{ SHA string } }
	err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/branches/"+pathEscape(ref), nil, &b)
	if err == nil {
		return b.Commit.SHA, true, nil
	}
	if !errors.Is(err, errNotFound) {
		return "", false, err
	}
	var c struct{ SHA string }
	if err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+pathEscape(ref), nil, &c); err != nil {
		return "", false, fmt.Errorf("revision %s: %w", ref, err)
	}
	return c.SHA, false, nil
}
