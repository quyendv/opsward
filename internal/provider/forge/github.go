package forge

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/quyendv/opsward/internal/capability"
)

type github struct{ *client }

func (g *github) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, bool, error) {
	var out struct{ Type, Encoding, Content string }
	err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+pathEscape(path)+"?ref="+url.QueryEscape(ref), nil, &out)
	if errors.Is(err, errNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	b, err := decodeContent(out.Encoding, out.Content)
	return b, err == nil, err
}

// Propose writes one commit through the Git data API: a tree on the base's tree, a
// commit on the base, and a branch at the commit.
func (g *github) Propose(ctx context.Context, repo string, p capability.Proposal) (string, error) {
	r := "/repos/" + repo
	var ref struct{ Object struct{ SHA string } }
	if err := g.do(ctx, http.MethodGet, r+"/git/ref/heads/"+pathEscape(p.Base), nil, &ref); err != nil {
		return "", err
	}
	var base struct{ Tree struct{ SHA string } }
	if err := g.do(ctx, http.MethodGet, r+"/git/commits/"+ref.Object.SHA, nil, &base); err != nil {
		return "", err
	}
	entries := make([]map[string]string, 0, len(p.Files))
	for path, content := range p.Files {
		entries = append(entries, map[string]string{"path": path, "mode": "100644", "type": "blob", "content": string(content)})
	}
	var tree, commit struct{ SHA string }
	if err := g.do(ctx, http.MethodPost, r+"/git/trees", map[string]any{"base_tree": base.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	if err := g.do(ctx, http.MethodPost, r+"/git/commits", map[string]any{"message": p.Message, "tree": tree.SHA, "parents": []string{ref.Object.SHA}}, &commit); err != nil {
		return "", err
	}
	if err := g.do(ctx, http.MethodPost, r+"/git/refs", map[string]any{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}, nil); err != nil {
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
