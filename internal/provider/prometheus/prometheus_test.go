package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/confine"
	"github.com/quyendv/opsward/internal/provider/httpapi"
)

type fakeProm struct {
	t       *testing.T
	queries []string
	paths   []string
	reply   string
}

func (f *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("method = %s, want POST", r.Method)
	}
	q := r.FormValue("query")
	f.queries = append(f.queries, q)
	f.paths = append(f.paths, r.URL.Path)
	if !strings.Contains(q, `namespace="shop"`) || !strings.Contains(q, `cluster="prod-a"`) {
		f.t.Errorf("unconfined query reached the backend: %s", q)
	}
	_, _ = w.Write([]byte(f.reply))
}

func source(t *testing.T, f *fakeProm) (*Source, func()) {
	t.Helper()
	srv := httptest.NewServer(f)
	src, err := NewSource(Options{
		HTTP:           httpapi.Options{URL: srv.URL},
		NamespaceLabel: "namespace", ClusterLabel: "cluster",
	}, map[string]string{"production": "prod-a"})
	if err != nil {
		t.Fatal(err)
	}
	return src, srv.Close
}

func query(t *testing.T, src *Source, q capability.MetricsQuery) (*capability.MetricsResult, error) {
	t.Helper()
	m, err := src.Metrics("production")
	if err != nil {
		t.Fatal(err)
	}
	return m.Query(context.Background(), "shop", q)
}

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func TestInstantVector(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"pod":"api-a"},"value":[1790000000,"0.25"]},
		{"metric":{"pod":"api-b"},"value":[1790000000,"NaN"]}]}}`}
	src, done := source(t, f)
	defer done()
	res, err := query(t, src, capability.MetricsQuery{Query: "up", Time: t0})
	if err != nil {
		t.Fatal(err)
	}
	if f.paths[0] != "/api/v1/query" || !strings.Contains(res.Query, `namespace="shop"`) {
		t.Fatalf("path %s, query %s", f.paths[0], res.Query)
	}
	if len(res.Series) != 2 || *res.Series[0].Last != 0.25 || res.Series[0].Labels["pod"] != "api-a" || len(res.Series[1].Points) != 0 {
		t.Fatalf("series = %+v", res.Series)
	}
}

func TestRangeMatrixWithSummary(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"pod":"api-a"},"values":[[1790000000,"1"],[1790000060,"+Inf"],[1790000120,"3"],[1790000180,"2"]]}]}}`}
	src, done := source(t, f)
	defer done()
	res, err := query(t, src, capability.MetricsQuery{Query: "rate(x[5m])", Range: true, Start: t0, End: t0.Add(time.Hour), Step: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Series[0]
	if f.paths[0] != "/api/v1/query_range" || len(s.Points) != 3 || *s.Min != 1 || *s.Max != 3 || *s.Last != 2 || *s.Avg != 2 || s.Samples != 3 {
		t.Fatalf("path %s series %+v", f.paths[0], s)
	}
}

func TestScalarAndSeriesCap(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"success","data":{"resultType":"scalar","result":[1790000000,"7"]}}`}
	src, done := source(t, f)
	defer done()
	res, err := query(t, src, capability.MetricsQuery{Query: "scalar(up)", Time: t0})
	if err != nil || len(res.Series) != 1 || *res.Series[0].Last != 7 {
		t.Fatalf("scalar = %+v, %v", res, err)
	}

	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"vector","result":[`)
	for i := range 60 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"metric":{"pod":"p%d"},"value":[1790000000,"1"]}`, i)
	}
	b.WriteString(`]}}`)
	f.reply = b.String()
	res, err = query(t, src, capability.MetricsQuery{Query: "up", Time: t0})
	if err != nil || len(res.Series) != maxSeries || res.SeriesOmitted != 60-maxSeries {
		t.Fatalf("cap: %d series, %d omitted, %v", len(res.Series), res.SeriesOmitted, err)
	}
}

func TestOutOfScopeQueryNeverReachesTheBackend(t *testing.T) {
	f := &fakeProm{t: t}
	src, done := source(t, f)
	defer done()
	if _, err := query(t, src, capability.MetricsQuery{Query: `up{namespace="other"}`, Time: t0}); !errors.Is(err, confine.ErrOutOfScope) {
		t.Fatalf("err = %v", err)
	}
	if len(f.queries) != 0 {
		t.Fatalf("backend received %v", f.queries)
	}
}

func TestBackendErrorAndUnknownEnvironment(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"error","errorType":"bad_data","error":"parse error at char 3"}`}
	src, done := source(t, f)
	defer done()
	if _, err := query(t, src, capability.MetricsQuery{Query: "up", Time: t0}); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("err = %v", err)
	}
	if _, err := src.Metrics("staging"); err == nil {
		t.Fatal("an environment without a cluster value must be an error")
	}
	src.Update(map[string]string{"staging": "stg"})
	if _, err := src.Metrics("staging"); err != nil {
		t.Fatalf("after Update: %v", err)
	}
}

// An instant query over a range vector (foo[1d]) returns a matrix; it gets the same
// points cap as a range query, keeping the newest points.
func TestPointsAreCappedForEveryResult(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"a"},"values":[`)
	for i := range 100 {
		if i > 0 {
			b.WriteString(",")
		}
		v := i
		if i == 10 {
			v = 1000 // the maximum is among the dropped points
		}
		fmt.Fprintf(&b, `[%d,"%d"]`, 1790000000+i*60, v)
	}
	b.WriteString(`]}]}}`)
	f := &fakeProm{t: t, reply: b.String()}
	src, done := source(t, f)
	defer done()
	res, err := query(t, src, capability.MetricsQuery{Query: "foo[1d]", Time: t0})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Series[0]
	if len(s.Points) != maxPoints || s.PointsOmitted != 100-maxPoints || *s.Last != 99 || s.Points[0].Value != 40 || *s.Max != 1000 {
		t.Fatalf("points %d omitted %d last %v first %v max %v", len(s.Points), s.PointsOmitted, *s.Last, s.Points[0].Value, *s.Max)
	}
}

// Issue #14: a series with no finite sample has no summary, rather than zeros that read
// as real values.
func TestNonFiniteSeriesHasNoSummary(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":"a"},"value":[1790000000,"NaN"]}]}}`}
	src, done := source(t, f)
	defer done()
	res, err := query(t, src, capability.MetricsQuery{Query: "up", Time: t0})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Series[0])
	if strings.Contains(string(b), `"min"`) || strings.Contains(string(b), `"last"`) {
		t.Fatalf("series = %s", b)
	}
}

// An empty namespace means every namespace only when the query says so: a tool that
// forgets a namespace must not go unconfined.
func TestEmptyNamespaceNeedsAllNamespaces(t *testing.T) {
	f := &fakeProm{t: t, reply: `{"status":"success","data":{"resultType":"vector","result":[]}}`}
	src, done := source(t, f)
	defer done()
	m, _ := src.Metrics("production")
	if _, err := m.Query(context.Background(), "", capability.MetricsQuery{Query: "up"}); err == nil || len(f.paths) != 0 {
		t.Fatalf("err = %v, requests %d", err, len(f.paths))
	}
}
