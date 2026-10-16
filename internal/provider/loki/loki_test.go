package loki

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/confine"
	"github.com/quyendv/opsward/internal/provider/httpapi"
)

type fakeLoki struct {
	t       *testing.T
	params  []url.Values
	headers []http.Header
	reply   string
}

func (f *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.params = append(f.params, r.URL.Query())
	f.headers = append(f.headers, r.Header.Clone())
	if r.URL.Path != "/loki/api/v1/query_range" {
		f.t.Errorf("path = %s", r.URL.Path)
	}
	if q := r.URL.Query().Get("query"); !strings.HasPrefix(q, `{app="api", namespace="shop"}`) {
		f.t.Errorf("unconfined query reached the backend: %s", q)
	}
	_, _ = w.Write([]byte(f.reply))
}

func TestQueryGroupsStreamsOldestFirst(t *testing.T) {
	f := &fakeLoki{t: t, reply: `{"status":"success","data":{"resultType":"streams","result":[
		{"stream":{"pod":"api-a","container":"app"},"values":[["1790000002000000000","third"],["1790000001000000000","second"]]},
		{"stream":{"pod":"api-b","container":"app"},"values":[["1790000000000000000","first"]]}]}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	src, err := NewSource(Options{HTTP: httpapi.Options{URL: srv.URL}, NamespaceLabel: "namespace"}, map[string]string{"production": "prod-a"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := src.Logs("production")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	res, err := l.Query(context.Background(), "shop", capability.LogsQuery{Query: `{app="api"} |= "x"`, Since: time.Hour, Limit: 100, End: end})
	if err != nil {
		t.Fatal(err)
	}
	p := f.params[0]
	wantEnd, wantStart := strconv.FormatInt(end.UnixNano(), 10), strconv.FormatInt(end.Add(-time.Hour).UnixNano(), 10)
	if p.Get("limit") != "100" || p.Get("direction") != "backward" || p.Get("end") != wantEnd || p.Get("start") != wantStart {
		t.Fatalf("params = %v", p)
	}
	if len(res.Streams) != 2 || res.Streams[0].Labels["pod"] != "api-a" {
		t.Fatalf("streams = %+v", res.Streams)
	}
	lines := res.Streams[0].Lines
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " second") || !strings.HasSuffix(lines[1], " third") || !strings.HasPrefix(lines[0], "2026-") {
		t.Fatalf("lines = %q", lines)
	}
	if !strings.Contains(res.Query, `namespace="shop"`) {
		t.Fatalf("query = %s", res.Query)
	}
}

func TestRejectedQueriesNeverReachTheBackend(t *testing.T) {
	f := &fakeLoki{t: t}
	srv := httptest.NewServer(f)
	defer srv.Close()
	src, _ := NewSource(Options{HTTP: httpapi.Options{URL: srv.URL}, NamespaceLabel: "namespace"}, map[string]string{"production": "prod-a"})
	l, _ := src.Logs("production")
	for q, want := range map[string]error{
		`rate({app="api"}[5m])`: confine.ErrNotLogQuery,
		`{namespace="other"}`:   confine.ErrOutOfScope,
	} {
		if _, err := l.Query(context.Background(), "shop", capability.LogsQuery{Query: q, Since: time.Hour, Limit: 10}); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", q, err, want)
		}
	}
	if len(f.params) != 0 {
		t.Fatalf("backend received %d requests", len(f.params))
	}
	if _, err := src.Logs("staging"); err == nil {
		t.Fatal("unknown environment accepted")
	}
}

// Issue #16: streams come with the most recent activity first.
func TestStreamsAreSortedByNewestLine(t *testing.T) {
	f := &fakeLoki{t: t, reply: `{"status":"success","data":{"resultType":"streams","result":[
		{"stream":{"pod":"old"},"values":[["1790000001000000000","a"]]},
		{"stream":{"pod":"new"},"values":[["1790000009000000000","b"],["1790000000000000000","c"]]}]}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	src, _ := NewSource(Options{HTTP: httpapi.Options{URL: srv.URL}, NamespaceLabel: "namespace"}, map[string]string{"production": "prod-a"})
	l, _ := src.Logs("production")
	res, err := l.Query(context.Background(), "shop", capability.LogsQuery{Query: `{app="api"}`, Since: time.Hour, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Streams[0].Labels["pod"] != "new" {
		t.Fatalf("first stream = %v", res.Streams[0].Labels)
	}
}

// Loki 3 moves per-line metadata (trace IDs, pod UIDs, file paths) out of stream labels
// when asked; without that, every line becomes a stream of its own.
func TestQueryAsksForCategorizedLabels(t *testing.T) {
	f := &fakeLoki{t: t, reply: `{"status":"success","data":{"resultType":"streams","encodingFlags":["categorize-labels"],"result":[
		{"stream":{"pod":"api-a","container":"app"},"values":[
			["1790000002000000000","second",{"structuredMetadata":{"trace_id":"b"}}],
			["1790000001000000000","first",{"structuredMetadata":{"trace_id":"a"}}]]}]}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	src, err := NewSource(Options{HTTP: httpapi.Options{URL: srv.URL}, NamespaceLabel: "namespace"}, map[string]string{"production": "prod-a"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := src.Logs("production")
	res, err := l.Query(context.Background(), "shop", capability.LogsQuery{Query: `{app="api"}`, Since: time.Hour, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.headers[0].Get("X-Loki-Response-Encoding-Flags"); got != "categorize-labels" {
		t.Fatalf("encoding flags header = %q", got)
	}
	if len(res.Streams) != 1 || len(res.Streams[0].Lines) != 2 || !strings.HasSuffix(res.Streams[0].Lines[0], " first") {
		t.Fatalf("streams = %+v", res.Streams)
	}
}
