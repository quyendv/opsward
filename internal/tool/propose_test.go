package tool

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/source"
)

// memForge is one forge's repositories in memory: repo -> ref -> path -> content.
type memForge struct {
	mu        sync.Mutex
	repos     map[string]map[string]map[string]string
	proposals []capability.Proposal
	calls     []string
}

func (f *memForge) DefaultBranch(_ context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "default:"+repo)
	return "main", nil
}

// ResolveRef treats every ref as its own commit id; the refs the repository keeps are
// its branches.
func (f *memForge) ResolveRef(_ context.Context, repo, ref string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ref:"+repo+"@"+ref)
	_, branch := f.repos[repo][ref]
	return ref, branch, nil
}

func (f *memForge) ReadFile(_ context.Context, repo, ref, p string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read:"+repo+"@"+ref+":"+p)
	c, ok := f.repos[repo][ref][p]
	return []byte(c), ok, nil
}

func (f *memForge) ListFiles(_ context.Context, repo, ref, dir string) ([]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.repos[repo][ref] {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, false, nil
}

func (f *memForge) Propose(_ context.Context, repo string, p capability.Proposal) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "propose:"+repo)
	f.proposals = append(f.proposals, p)
	return "https://git.example.com/" + repo + "/pulls/1", nil
}

type staticChanges struct{ c *Changes }

func (s staticChanges) Changes() *Changes { return s.c }

const values = "image: api:1.4.2\nreplicaCount: 2\nresources:\n  limits: { memory: 256Mi }\ndb:\n  password: hunter2hunter2\n"

func changeFixture(strategy string) (*fakeSource, *memForge, ChangeConfig) {
	src := &fakeSource{
		workload:    &capability.Workload{Labels: map[string]string{"app": "api"}},
		annotations: map[string]string{"opsward/source": "https://git.example.com/team-a/gitops.git//apps/api"},
	}
	fg := &memForge{repos: map[string]map[string]map[string]string{
		"team-a/gitops": {"main": {
			"apps/api/values.yaml":           values,
			"apps/api/templates/deploy.yaml": "kind: Deployment\n",
			"apps/api/templates/secret.yaml": secretManifest,
			"apps/web/values.yaml":           "replicaCount: 1\n",
		}},
	}}
	cfg := staticChanges{&Changes{
		Strategies: map[string]string{"staging": strategy},
		Resolve:    source.Options{AnnotationKeys: []string{"opsward/source"}},
		Forges:     []ForgeRoute{{Name: "gitea", Hosts: []string{"git.example.com"}, Repositories: []string{"team-a/gitops"}, Forge: fg}},
	}}
	return src, fg, cfg
}

const secretManifest = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: api\n  labels: { tier: backend }\ndata:\n  key: QUFBQUFBQUE=\n" //nolint:gosec // G101: a fixture whose value must not reach the caller

const target = `"environment":"staging","namespace":"shop","kind":"Deployment","name":"api"`

func TestResolveSourceListsAndReads(t *testing.T) {
	src, _, cfg := changeFixture("suggest")
	tl, err := ResolveSource(src, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Risk != core.RiskRead {
		t.Fatalf("risk = %s", tl.Risk)
	}
	p, err := tl.Prepare(json.RawMessage(`{` + target + `,"files":["apps/api/values.yaml"]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	v := out.(*SourceView)
	if v.Resolver != "annotation" || v.Strategy != "suggest" || len(v.Locations) != 1 {
		t.Fatalf("view = %+v", v)
	}
	l := v.Locations[0]
	if l.Revision != "main" || !slices.Equal(l.Files, []string{"apps/api/templates/deploy.yaml", "apps/api/templates/secret.yaml", "apps/api/values.yaml"}) {
		t.Fatalf("location = %+v", l)
	}
	if len(v.Contents) != 1 || v.Contents[0].Content != values {
		t.Fatalf("contents = %+v", v.Contents)
	}

	p, _ = tl.Prepare(json.RawMessage(`{` + target + `,"files":["apps/web/values.yaml"]}`))
	if _, err := p.Run(context.Background(), "reader"); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("a file outside the source: err = %v", err)
	}
}

func TestProposeChangeSuggests(t *testing.T) {
	src, fg, cfg := changeFixture("suggest")
	tl, err := ProposeChange(src, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Risk != core.RiskWrite {
		t.Fatalf("declared risk = %s, want write: the highest it can have", tl.Risk)
	}
	p, err := tl.Prepare(json.RawMessage(`{` + target + `,"title":"Scale api to 4","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 4"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Risk != core.RiskRead {
		t.Fatalf("suggest risk = %q, want read", p.Risk)
	}
	out, err := p.Run(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	ch := out.(*core.Change)
	if ch.Strategy != "suggest" || ch.URL != "" {
		t.Fatalf("change = %+v", ch)
	}
	for _, want := range []string{"--- a/apps/api/values.yaml", "+++ b/apps/api/values.yaml", "-replicaCount: 2", "+replicaCount: 4"} {
		if !strings.Contains(ch.Diff, want) {
			t.Errorf("diff misses %q:\n%s", want, ch.Diff)
		}
	}
	if strings.Contains(ch.Diff, "hunter2") {
		t.Errorf("diff carries a secret:\n%s", ch.Diff)
	}
	if len(fg.proposals) != 0 {
		t.Fatal("suggest proposed")
	}
}

func TestProposeChangeOpensAPullRequest(t *testing.T) {
	src, fg, cfg := changeFixture("pull-request")
	tl, _ := ProposeChange(src, cfg)
	p, err := tl.Prepare(json.RawMessage(`{` + target + `,"title":"Scale api to 4","description":"Load doubled.","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 4"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Risk != "" {
		t.Fatalf("pull-request risk = %q, want the tool's", p.Risk)
	}
	pre, err := p.Preview(context.Background(), "operator")
	if err != nil || pre.Strategy != "pull-request" || !strings.Contains(pre.Diff, "+replicaCount: 4") || len(fg.proposals) != 0 {
		t.Fatalf("preview = %+v, %v, proposals %d", pre, err, len(fg.proposals))
	}
	ctx := core.WithAuditID(core.WithPrincipal(context.Background(), core.Principal{Subject: "alice"}), "0192-intent")
	out, err := p.Run(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	ch := out.(*core.Change)
	if ch.URL == "" || ch.Diff != pre.Diff {
		t.Fatalf("change = %+v", ch)
	}
	if len(fg.proposals) != 1 {
		t.Fatalf("proposals = %d", len(fg.proposals))
	}
	pr := fg.proposals[0]
	if pr.Base != "main" || pr.BaseCommit != "main" || pr.Branch != "opsward/0192-intent" || pr.Title != "Scale api to 4" {
		t.Fatalf("proposal = %+v", pr)
	}
	for _, want := range []string{"Load doubled.", "alice", "0192-intent", "staging", "shop"} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("body misses %q:\n%s", want, pr.Body)
		}
	}
	got := string(pr.Files["apps/api/values.yaml"])
	if got != strings.Replace(values, "replicaCount: 2", "replicaCount: 4", 1) {
		t.Fatalf("new content = %q: the secret must be written back unredacted", got)
	}
}

func TestProposeChangeRefuses(t *testing.T) {
	edit := func(path, old, new string) string {
		b, _ := json.Marshal([]map[string]string{{"path": path, "old": old, "new": new}})
		return `{` + target + `,"title":"t","edits":` + string(b) + `}`
	}
	tests := []struct{ name, args, want string }{
		{"old not found", edit("apps/api/values.yaml", "replicaCount: 3", "replicaCount: 4"), "not found"},
		{"old not unique", edit("apps/api/values.yaml", "i", "I"), "occurs"},
		{"outside the source", edit("apps/web/values.yaml", "replicaCount: 1", "replicaCount: 2"), "outside"},
		{"missing file", edit("apps/api/missing.yaml", "a", "b"), "does not exist"},
		{"no change", edit("apps/api/values.yaml", "replicaCount: 2", "replicaCount: 2"), "changes nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _, cfg := changeFixture("suggest")
			tl, _ := ProposeChange(src, cfg)
			p, err := tl.Prepare(json.RawMessage(tt.args))
			if err == nil {
				_, err = p.Run(context.Background(), "reader")
			}
			if !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want invalid input with %q", err, tt.want)
			}
		})
	}
}

func TestProposeChangeNeedsAListedRepository(t *testing.T) {
	src, fg, cfg := changeFixture("suggest")
	src.annotations["opsward/source"] = "https://git.example.com/team-b/gitops.git//apps/api"
	tl, _ := ProposeChange(src, cfg)
	p, _ := tl.Prepare(json.RawMessage(`{` + target + `,"title":"t","edits":[{"path":"apps/api/values.yaml","old":"a","new":"b"}]}`))
	_, err := p.Run(context.Background(), "reader")
	if err == nil || !strings.Contains(err.Error(), "team-b/gitops") || len(fg.calls) != 0 {
		t.Fatalf("err = %v, forge calls %v", err, fg.calls)
	}
}

func TestProposeChangeRefusesACommitForAPullRequest(t *testing.T) {
	src, _, cfg := changeFixture("pull-request")
	src.annotations["opsward/source"] = "https://git.example.com/team-a/gitops.git//apps/api@0123456789abcdef0123456789abcdef01234567"
	tl, _ := ProposeChange(src, cfg)
	p, _ := tl.Prepare(json.RawMessage(`{` + target + `,"title":"t","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 4"}]}`))
	if _, err := p.Preview(context.Background(), "operator"); !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), "not a branch") {
		t.Fatalf("err = %v", err)
	}
}

// Without a source there is nothing to edit: the call says so and proposes nothing,
// whatever the strategy.
func TestProposeChangeFallsBackToSuggest(t *testing.T) {
	src, fg, cfg := changeFixture("pull-request")
	src.annotations = nil
	tl, _ := ProposeChange(src, cfg)
	p, _ := tl.Prepare(json.RawMessage(`{` + target + `,"title":"t","edits":[{"path":"apps/api/values.yaml","old":"replicaCount: 2","new":"replicaCount: 4"}]}`))
	out, err := p.Run(core.WithAuditID(context.Background(), "x"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	ch := out.(*core.Change)
	if ch.Strategy != "suggest" || !strings.Contains(ch.Note, "source not resolved") || len(fg.proposals) != 0 {
		t.Fatalf("change = %+v, proposals %d", ch, len(fg.proposals))
	}
}

// Secret manifests in the source keep their keys and lose their values, in contents and
// in diffs.
func TestSecretManifestValuesStayOut(t *testing.T) {
	src, fg, cfg := changeFixture("pull-request")
	rs, _ := ResolveSource(src, cfg)
	p, _ := rs.Prepare(json.RawMessage(`{` + target + `,"files":["apps/api/templates/secret.yaml"]}`))
	out, err := p.Run(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	if c := out.(*SourceView).Contents[0].Content; strings.Contains(c, "QUFBQUFBQUE") || !strings.Contains(c, "key: [REDACTED]") {
		t.Fatalf("content = %q", c)
	}
	pc, _ := ProposeChange(src, cfg)
	p, _ = pc.Prepare(json.RawMessage(`{` + target + `,"title":"t","edits":[{"path":"apps/api/templates/secret.yaml","old":"tier: backend","new":"tier: api"}]}`))
	ch, err := p.Preview(context.Background(), "operator")
	if err != nil || strings.Contains(ch.Diff, "QUFBQUFBQUE") || !strings.Contains(ch.Diff, "+  labels: { tier: api }") {
		t.Fatalf("diff = %q, %v", ch.Diff, err)
	}
	if _, err := p.Run(core.WithAuditID(context.Background(), "x"), "operator"); err != nil {
		t.Fatal(err)
	}
	if got := string(fg.proposals[0].Files["apps/api/templates/secret.yaml"]); !strings.Contains(got, "key: QUFBQUFBQUE=") {
		t.Fatalf("the proposal lost the secret's value: %q", got)
	}
}

func TestDiffHasNoLineTheFileLacks(t *testing.T) {
	src, _, cfg := changeFixture("suggest")
	tl, _ := ProposeChange(src, cfg)
	p, _ := tl.Prepare(json.RawMessage(`{` + target + `,"title":"t","edits":[{"path":"apps/api/templates/deploy.yaml","old":"Deployment","new":"StatefulSet"}]}`))
	out, err := p.Run(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/apps/api/templates/deploy.yaml\n+++ b/apps/api/templates/deploy.yaml\n@@ -1 +1 @@\n-kind: Deployment\n+kind: StatefulSet\n"
	if d := out.(*core.Change).Diff; d != want {
		t.Fatalf("diff = %q, want %q", d, want)
	}
}

func TestLinesEndEveryLine(t *testing.T) {
	for in, want := range map[string][]string{
		"a\nb\n": {"a\n", "b\n"},
		"a\nb":   {"a\n", "b\n"}, // no final newline: the diff still has one line per line
		"":       {},
	} {
		if got := lines(in); !slices.Equal(got, want) {
			t.Errorf("lines(%q) = %q, want %q", in, got, want)
		}
	}
}
