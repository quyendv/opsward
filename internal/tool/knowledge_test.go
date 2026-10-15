package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/knowledge"
)

func library(t *testing.T) *knowledge.Library {
	t.Helper()
	p, _, err := knowledge.Load(knowledge.Options{Builtin: true})
	if err != nil {
		t.Fatal(err)
	}
	return knowledge.NewLibrary(p)
}

func run(t *testing.T, tl engine.Tool, args string) (any, error) {
	t.Helper()
	p, err := tl.Prepare(json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	if p.Scope.Environment != "" || p.Resolve != nil {
		t.Fatalf("knowledge tools take no environment: %+v", p.Scope)
	}
	return p.Run(context.Background(), engine.ProfileReader)
}

func TestSearchKnowledge(t *testing.T) {
	tl, err := SearchKnowledge(library(t))
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name != "search_knowledge" || !tl.Global {
		t.Fatalf("tool = %s global=%v", tl.Name, tl.Global)
	}
	out, err := run(t, tl, `{"query":"container keeps crashing"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.HasPrefix(string(b), `[{"id":"crashloop-backoff"`) {
		t.Fatalf("hits = %s", b)
	}
	for _, bad := range []string{`{}`, `{"query":""}`, `{"query":"x","limit":21}`} {
		if _, err := tl.Prepare(json.RawMessage(bad)); err == nil {
			t.Errorf("Prepare(%s) succeeded", bad)
		}
	}
}

func TestGetKnowledge(t *testing.T) {
	tl, err := GetKnowledge(library(t))
	if err != nil {
		t.Fatal(err)
	}
	if tl.Name != "get_knowledge" || !tl.Global {
		t.Fatalf("tool = %s global=%v", tl.Name, tl.Global)
	}
	out, err := run(t, tl, `{"id":"failed-scheduling"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"body":`) || !strings.Contains(string(b), "Taints") {
		t.Fatalf("doc = %s", b)
	}
	if _, err := run(t, tl, `{"id":"nope"}`); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown id: err = %v", err)
	}
}
