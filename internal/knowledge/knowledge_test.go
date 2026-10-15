package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/core"
)

const crashDoc = `---
id: my-crash
title: Something crashes
severity: warning
applies-to:
  tools: [get_workload_status]
  when: 'result.pods.exists(p, has(p.waiting) && p.waiting == "CrashLoopBackOff")'
---
First paragraph
spans two lines.

Second paragraph.
`

func TestParse(t *testing.T) {
	d, err := parse("x.md", []byte(crashDoc))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "my-crash" || d.Severity != "warning" || d.Summary != "First paragraph spans two lines." || len(d.Tools) != 1 {
		t.Fatalf("doc = %+v", d)
	}
	if !strings.Contains(d.Body, "Second paragraph.") {
		t.Fatalf("body = %q", d.Body)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tt := range map[string]struct{ doc, want string }{
		"no frontmatter":   {"just text\n", "frontmatter"},
		"unterminated":     {"---\nid: a\ntitle: b\n", "frontmatter"},
		"missing id":       {"---\ntitle: b\n---\nbody\n", "id is required"},
		"bad id":           {"---\nid: Bad_ID\ntitle: b\n---\nbody\n", "id"},
		"missing title":    {"---\nid: a\n---\nbody\n", "title is required"},
		"bad severity":     {"---\nid: a\ntitle: b\nseverity: urgent\n---\nbody\n", "severity"},
		"unknown field":    {"---\nid: a\ntitle: b\ncolour: red\n---\nbody\n", "colour"},
		"applies no tools": {"---\nid: a\ntitle: b\napplies-to: {when: 'true'}\n---\nbody\n", "applies-to.tools"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse("x.md", []byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBuiltinAndPaths(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/crash.md", crashDoc)
	write(t, dir, "notes.txt", "ignored, not markdown")
	pack, warnings, err := Load(Options{Builtin: true, Paths: []string{dir}, Tools: []string{"get_workload_status", "list_workload_events"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	for _, id := range []string{"my-crash", "crashloop-backoff", "image-pull-backoff", "failed-scheduling"} {
		if _, ok := pack.Get(id); !ok {
			t.Errorf("document %q not loaded", id)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	dup := t.TempDir()
	write(t, dup, "a.md", crashDoc)
	write(t, dup, "b.md", crashDoc)
	notBool := t.TempDir()
	write(t, notBool, "a.md", strings.Replace(crashDoc, `when: 'result.pods.exists(p, has(p.waiting) && p.waiting == "CrashLoopBackOff")'`, `when: 'tool.name'`, 1))
	for name, tt := range map[string]struct {
		dir, want string
	}{
		"duplicate id":   {dup, "duplicate id"},
		"when not bool":  {notBool, "bool"},
		"missing folder": {filepath.Join(t.TempDir(), "nope"), "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(Options{Paths: []string{tt.dir}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadWarnsAboutUnknownTools(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", strings.Replace(crashDoc, "tools: [get_workload_status]", "tools: [restart_everything]", 1))
	_, warnings, err := Load(Options{Paths: []string{dir}, Tools: []string{"get_workload_status"}})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "restart_everything") {
		t.Fatalf("warnings = %v, err = %v", warnings, err)
	}
}

func request(tool string) core.Request {
	return core.Request{Tool: tool, Risk: core.RiskRead, Scope: core.Scope{Environment: "staging", Name: "api"}}
}

func statusResult(waiting string) any {
	pod := map[string]any{"name": "api-a", "ready": false}
	if waiting != "" {
		pod["waiting"] = waiting
	}
	return map[string]any{"name": "api", "pods": []any{pod, map[string]any{"name": "api-b", "ready": true}}}
}

func TestAnnotate(t *testing.T) {
	pack, _, err := Load(Options{Builtin: true})
	if err != nil {
		t.Fatal(err)
	}
	notes := pack.Annotate(request("get_workload_status"), statusResult("CrashLoopBackOff"))
	if len(notes) != 1 || notes[0].ID != "crashloop-backoff" || notes[0].Summary == "" {
		t.Fatalf("crash loop notes = %+v", notes)
	}
	if notes := pack.Annotate(request("get_workload_status"), statusResult("")); len(notes) != 0 {
		t.Fatalf("healthy notes = %+v", notes)
	}
	if notes := pack.Annotate(request("list_workload_logs"), statusResult("CrashLoopBackOff")); len(notes) != 0 {
		t.Fatalf("other tool notes = %+v", notes)
	}
	if notes := pack.Annotate(request("get_workload_status"), "not an object"); len(notes) != 0 {
		t.Fatalf("a condition that fails to evaluate must not attach: %+v", notes)
	}
	events := []any{map[string]any{"reason": "FailedScheduling", "object": "Pod/api-a"}}
	if notes := pack.Annotate(request("list_workload_events"), events); len(notes) != 1 || notes[0].ID != "failed-scheduling" {
		t.Fatalf("events notes = %+v", notes)
	}
}

func TestSearch(t *testing.T) {
	pack, _, err := Load(Options{Builtin: true})
	if err != nil {
		t.Fatal(err)
	}
	hits := pack.Search("container keeps crashing", 5)
	if len(hits) == 0 || hits[0].ID != "crashloop-backoff" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits := pack.Search("pull image", 1); len(hits) != 1 || hits[0].ID != "image-pull-backoff" {
		t.Fatalf("limited hits = %+v", hits)
	}
	if hits := pack.Search("zzzz qqqq", 5); len(hits) != 0 {
		t.Fatalf("no-match hits = %+v", hits)
	}
}

func TestLibrarySwap(t *testing.T) {
	a, _, _ := Load(Options{Builtin: true})
	lib := NewLibrary(a)
	if _, ok := lib.Pack().Get("crashloop-backoff"); !ok {
		t.Fatal("builtin missing")
	}
	b, _, _ := Load(Options{})
	lib.Set(b)
	if _, ok := lib.Pack().Get("crashloop-backoff"); ok {
		t.Fatal("Set did not replace the pack")
	}
	if notes := lib.Annotate(request("get_workload_status"), statusResult("CrashLoopBackOff")); len(notes) != 0 {
		t.Fatalf("notes after swap = %+v", notes)
	}
}
