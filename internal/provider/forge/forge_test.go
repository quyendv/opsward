package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
)

// fakeHost keeps one repository's branches in memory and serves a forge's API over them.
type fakeHost struct {
	t        *testing.T
	mu       sync.Mutex
	branches map[string]map[string]string // branch -> path -> content
	prs      []map[string]any
	auth     []string
}

func newHost(t *testing.T) *fakeHost {
	return &fakeHost{t: t, branches: map[string]map[string]string{"main": {
		"apps/api/values.yaml":           "replicaCount: 2\n",
		"apps/api/templates/deploy.yaml": "kind: Deployment\n",
		"apps/web/values.yaml":           "replicaCount: 1\n",
	}}}
}

// baseSHA is the commit main points to; reads at it see main's files.
const baseSHA = "0123456789abcdef0123456789abcdef01234567"

// files returns the files at a branch or at baseSHA.
func (h *fakeHost) files(ref string) map[string]string {
	if ref == baseSHA {
		ref = "main"
	}
	return h.branches[ref]
}

func (h *fakeHost) branch(name string) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.branches[name]
}

func readJSON(r *http.Request) map[string]any {
	var v map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &v)
	return v
}

// tree lists a branch's files and the directories above them, as Git trees do.
func (h *fakeHost) tree(ref string) []map[string]any {
	var out []map[string]any
	dirs := map[string]bool{}
	for p := range h.files(ref) {
		out = append(out, map[string]any{"path": p, "type": "blob"})
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if !dirs[d] {
				dirs[d] = true
				out = append(out, map[string]any{"path": d, "type": "tree"})
			}
		}
	}
	// A stable order, so pages cut from separate calls do not overlap.
	slices.SortFunc(out, func(a, b map[string]any) int { return strings.Compare(a["path"].(string), b["path"].(string)) })
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func unb64(t *testing.T, s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// github serves the GitHub REST endpoints the forge uses.
func (h *fakeHost) github() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		p := strings.TrimPrefix(r.URL.Path, "/repos/example-org/gitops")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/contents/"):
			c, ok := h.files(r.URL.Query().Get("ref"))[strings.TrimPrefix(p, "/contents/")]
			if !ok {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"type": "file", "encoding": "base64", "content": b64(c)})
		case r.Method == http.MethodGet && p == "":
			writeJSON(w, map[string]any{"default_branch": "main"})
		case r.Method == http.MethodGet && p == "/git/trees/"+baseSHA && r.URL.Query().Get("recursive") == "1":
			writeJSON(w, map[string]any{"tree": h.tree(baseSHA), "truncated": false})
		case r.Method == http.MethodGet && p == "/branches/release/1.0":
			writeJSON(w, map[string]any{"commit": map[string]any{"sha": baseSHA}})
		case r.Method == http.MethodGet && p == "/branches/main":
			writeJSON(w, map[string]any{"commit": map[string]any{"sha": baseSHA}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/branches/"):
			http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && p == "/commits/v1.0":
			writeJSON(w, map[string]any{"sha": baseSHA})
		case r.Method == http.MethodGet && p == "/git/commits/"+baseSHA:
			writeJSON(w, map[string]any{"tree": map[string]any{"sha": "base-tree"}})
		case r.Method == http.MethodPost && p == "/git/trees":
			body := readJSON(r)
			if body["base_tree"] != "base-tree" {
				h.t.Errorf("base_tree = %v", body["base_tree"])
			}
			files := map[string]string{}
			for k, v := range h.branches["main"] {
				files[k] = v
			}
			for _, e := range body["tree"].([]any) {
				m := e.(map[string]any)
				files[m["path"].(string)] = m["content"].(string)
			}
			h.branches["pending"] = files
			writeJSON(w, map[string]any{"sha": "new-tree"})
		case r.Method == http.MethodPost && p == "/git/commits":
			body := readJSON(r)
			if body["tree"] != "new-tree" || body["parents"].([]any)[0] != baseSHA {
				h.t.Errorf("commit = %v", body)
			}
			writeJSON(w, map[string]any{"sha": "new-commit"})
		case r.Method == http.MethodPost && p == "/git/refs":
			body := readJSON(r)
			h.branches[strings.TrimPrefix(body["ref"].(string), "refs/heads/")] = h.branches["pending"]
			writeJSON(w, map[string]any{"ref": body["ref"]})
		case r.Method == http.MethodPost && p == "/pulls":
			body := readJSON(r)
			h.prs = append(h.prs, body)
			writeJSON(w, map[string]any{"html_url": "https://github.example.com/example-org/gitops/pull/7"})
		default:
			h.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	})
}

// gitea serves the Gitea (Forgejo) REST endpoints the forge uses.
func (h *fakeHost) gitea() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/example-org/gitops")
		switch {
		case r.Method == http.MethodGet && p == "":
			writeJSON(w, map[string]any{"default_branch": "main"})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/contents/"):
			c, ok := h.files(r.URL.Query().Get("ref"))[strings.TrimPrefix(p, "/contents/")]
			if !ok {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"type": "file", "encoding": "base64", "content": b64(c), "sha": "blob-" + c[:3]})
		case r.Method == http.MethodGet && (p == "/branches/main" || p == "/branches/release/1.0"):
			writeJSON(w, map[string]any{"commit": map[string]any{"id": baseSHA}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/branches/"):
			http.Error(w, `{"message":"branch not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && p == "/commits" && r.URL.Query().Get("sha") == "v1.0":
			writeJSON(w, []map[string]any{{"sha": baseSHA}})
		case r.Method == http.MethodGet && p == "/git/trees/"+baseSHA && r.URL.Query().Get("recursive") == "true":
			// Two pages, to exercise paging.
			all, page := h.tree(baseSHA), r.URL.Query().Get("page")
			half := len(all) / 2
			if page == "1" {
				writeJSON(w, map[string]any{"tree": all[:half], "truncated": true, "page": 1})
			} else {
				writeJSON(w, map[string]any{"tree": all[half:], "truncated": false, "page": 2})
			}
		case r.Method == http.MethodPost && p == "/contents":
			body := readJSON(r)
			files := map[string]string{}
			for k, v := range h.branches[body["branch"].(string)] {
				files[k] = v
			}
			for _, e := range body["files"].([]any) {
				m := e.(map[string]any)
				if m["operation"] != "update" || m["sha"] == "" {
					h.t.Errorf("file op = %v", m)
				}
				files[m["path"].(string)] = unb64(h.t, m["content"].(string))
			}
			h.branches[body["new_branch"].(string)] = files
			writeJSON(w, map[string]any{"commit": map[string]any{"sha": "c"}})
		case r.Method == http.MethodPost && p == "/pulls":
			h.prs = append(h.prs, readJSON(r))
			writeJSON(w, map[string]any{"html_url": "https://git.example.com/example-org/gitops/pulls/3"})
		default:
			h.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	})
}

// gitlab serves the GitLab REST endpoints the forge uses.
func (h *fakeHost) gitlab() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.auth = append(h.auth, r.Header.Get("PRIVATE-TOKEN"))
		p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/example-org%2Fgitops")
		switch {
		case r.Method == http.MethodGet && p == "":
			writeJSON(w, map[string]any{"default_branch": "main"})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repository/files/"):
			file, _ := url.PathUnescape(strings.TrimPrefix(p, "/repository/files/"))
			c, ok := h.files(r.URL.Query().Get("ref"))[file]
			if !ok {
				http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"encoding": "base64", "content": b64(c)})
		case r.Method == http.MethodGet && p == "/repository/tree":
			q := r.URL.Query()
			if q.Get("ref") != baseSHA || q.Get("recursive") != "true" {
				h.t.Errorf("tree query = %v", q)
			}
			var out []map[string]any
			for _, e := range h.tree(baseSHA) {
				if strings.HasPrefix(e["path"].(string), q.Get("path")+"/") {
					out = append(out, e)
				}
			}
			if q.Get("page") != "1" {
				out = nil
			}
			writeJSON(w, out)
		case r.Method == http.MethodGet && (p == "/repository/branches/main" || p == "/repository/branches/release%2F1.0"):
			writeJSON(w, map[string]any{"commit": map[string]any{"id": baseSHA}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repository/branches/"):
			http.Error(w, `{"message":"404 Branch Not Found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && p == "/repository/commits/v1.0":
			writeJSON(w, map[string]any{"id": baseSHA})
		case r.Method == http.MethodPost && p == "/repository/commits":
			body := readJSON(r)
			if body["start_sha"] != baseSHA || body["start_branch"] != nil {
				h.t.Errorf("commit start = %v / %v", body["start_sha"], body["start_branch"])
			}
			files := map[string]string{}
			for k, v := range h.files(baseSHA) {
				files[k] = v
			}
			for _, e := range body["actions"].([]any) {
				m := e.(map[string]any)
				if m["action"] != "update" {
					h.t.Errorf("action = %v", m)
				}
				files[m["file_path"].(string)] = unb64(h.t, m["content"].(string))
			}
			h.branches[body["branch"].(string)] = files
			writeJSON(w, map[string]any{"id": "c"})
		case r.Method == http.MethodPost && p == "/merge_requests":
			h.prs = append(h.prs, readJSON(r))
			writeJSON(w, map[string]any{"web_url": "https://gitlab.example.com/example-org/gitops/-/merge_requests/9"})
		default:
			h.t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	})
}

func TestForges(t *testing.T) {
	for _, kind := range []string{"github", "gitea", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			h := newHost(t)
			var handler http.Handler
			switch kind {
			case "github":
				handler = h.github()
			case "gitea":
				handler = h.gitea()
			default:
				handler = h.gitlab()
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			f, err := New(Options{Type: kind, URL: srv.URL, Token: func() (string, error) { return "s3cret", nil }})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if b, err := f.DefaultBranch(ctx, "example-org/gitops"); err != nil || b != "main" {
				t.Fatalf("DefaultBranch = %q, %v", b, err)
			}
			for ref, want := range map[string]bool{"main": true, "release/1.0": true, "v1.0": false} {
				c, branch, err := f.ResolveRef(ctx, "example-org/gitops", ref)
				if err != nil || c != baseSHA || branch != want {
					t.Fatalf("ResolveRef(%s) = %q, %v, %v", ref, c, branch, err)
				}
			}
			got, found, err := f.ReadFile(ctx, "example-org/gitops", baseSHA, "apps/api/values.yaml")
			if err != nil || !found || string(got) != "replicaCount: 2\n" {
				t.Fatalf("ReadFile = %q, %v, %v", got, found, err)
			}
			if _, found, err := f.ReadFile(ctx, "example-org/gitops", baseSHA, "missing.yaml"); err != nil || found {
				t.Fatalf("missing file: %v, %v", found, err)
			}
			files, truncated, err := f.ListFiles(ctx, "example-org/gitops", baseSHA, "apps/api")
			if err != nil || truncated || !slices.Equal(files, []string{"apps/api/templates/deploy.yaml", "apps/api/values.yaml"}) {
				t.Fatalf("ListFiles = %v, %v, %v", files, truncated, err)
			}
			u, err := f.Propose(ctx, "example-org/gitops", capability.Proposal{
				Base: "main", BaseCommit: baseSHA, Branch: "opsward/abc", Title: "Scale api", Body: "why", Message: "Scale api\n\nwhy",
				Files: map[string][]byte{"apps/api/values.yaml": []byte("replicaCount: 4\n")},
			})
			if err != nil || !strings.HasPrefix(u, "https://") {
				t.Fatalf("Propose = %q, %v", u, err)
			}
			if c := h.branch("opsward/abc")["apps/api/values.yaml"]; c != "replicaCount: 4\n" {
				t.Fatalf("branch content = %q", c)
			}
			if h.branch("main")["apps/api/values.yaml"] != "replicaCount: 2\n" || len(h.prs) != 1 {
				t.Fatalf("base changed or no pull request: %v", h.prs)
			}
			for _, a := range h.auth {
				if !strings.Contains(a, "s3cret") {
					t.Fatalf("request without the token: %q", a)
				}
			}
		})
	}
}

func TestForgeErrorsDoNotCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	f, _ := New(Options{Type: "github", URL: srv.URL, Token: func() (string, error) { return "s3cret", nil }})
	_, _, err := f.ReadFile(context.Background(), "example-org/gitops", "main", "x")
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// Paths and refs reach API URLs; a dot segment there could leave the repository, so no
// such request is sent, whatever the caller checked before.
func TestForgeRefusesDotSegments(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		writeJSON(w, map[string]any{})
	}))
	defer srv.Close()
	for _, kind := range []string{"github", "gitea", "gitlab"} {
		f, _ := New(Options{Type: kind, URL: srv.URL, Token: func() (string, error) { return "t", nil }})
		ctx := context.Background()
		if _, _, err := f.ReadFile(ctx, "example-org/gitops", "main", "../../other/private/x"); err == nil {
			t.Errorf("%s: ReadFile with .. was sent", kind)
		}
		// GitLab takes the ref as a query parameter, where it cannot change the path.
		if kind != "gitlab" {
			if _, _, err := f.ListFiles(ctx, "example-org/gitops", "../../other/x", "apps"); err == nil {
				t.Errorf("%s: ListFiles with a .. ref was sent", kind)
			}
		}
		if _, _, err := f.ReadFile(ctx, "../other/private", "main", "x"); err == nil {
			t.Errorf("%s: a .. repository was sent", kind)
		}
	}
	if hits != 0 {
		t.Fatalf("%d requests reached the forge", hits)
	}
}
