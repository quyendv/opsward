// Package prometheus implements capability.Metrics on the Prometheus HTTP API, which
// Prometheus, VictoriaMetrics, Mimir and Thanos all serve.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/confine"
	"github.com/quyendv/opsward/internal/provider/httpapi"
)

// Limits from docs/design/0004: an instant query over a range vector (foo[1d]) returns a
// matrix too, so the points cap applies to every result, not only to range queries.
const (
	maxSeries = 50
	maxPoints = 60
)

// Options configures the backend.
type Options struct {
	HTTP           httpapi.Options
	NamespaceLabel string
	ClusterLabel   string // empty: one cluster, no cluster matcher
}

// Source hands out Metrics per environment. Every query is confined to the call's
// namespace and the environment's cluster before it is sent.
type Source struct {
	client *httpapi.Client
	opt    Options

	mu       sync.RWMutex
	clusters map[string]string // environment -> cluster label value
}

var _ capability.MetricsSource = (*Source)(nil)

// NewSource builds the source; clusters maps each environment to its cluster label value.
func NewSource(opt Options, clusters map[string]string) (*Source, error) {
	c, err := httpapi.New(opt.HTTP)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return &Source{client: c, opt: opt, clusters: clusters}, nil
}

// Update replaces the environment to cluster value map, as on a configuration reload.
func (s *Source) Update(clusters map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters = clusters
}

// Metrics implements capability.MetricsSource.
func (s *Source) Metrics(environment string) (capability.Metrics, error) {
	s.mu.RLock()
	cluster, ok := s.clusters[environment]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("metrics: unknown environment %q", environment)
	}
	return &metrics{s: s, cluster: cluster}, nil
}

type metrics struct {
	s       *Source
	cluster string
}

func (m *metrics) Query(ctx context.Context, namespace string, q capability.MetricsQuery) (*capability.MetricsResult, error) {
	if namespace == "" && !q.AllNamespaces {
		return nil, errors.New("metrics: no namespace, and the query is not marked as spanning every namespace")
	}
	confined, err := confine.PromQL(q.Query, confine.Scope{
		NamespaceLabel: m.s.opt.NamespaceLabel, Namespace: namespace,
		ClusterLabel: m.s.opt.ClusterLabel, Cluster: m.cluster,
	})
	if err != nil {
		return nil, err
	}
	params := url.Values{"query": {confined}}
	path := "/api/v1/query"
	if q.Range {
		path = "/api/v1/query_range"
		params.Set("start", unix(q.Start))
		params.Set("end", unix(q.End))
		params.Set("step", strconv.FormatFloat(q.Step.Seconds(), 'f', -1, 64))
	} else if !q.Time.IsZero() {
		params.Set("time", unix(q.Time))
	}
	body, err := m.s.client.Post(ctx, path, params)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	series, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	res := &capability.MetricsResult{Query: confined, Series: series}
	if len(res.Series) > maxSeries {
		res.SeriesOmitted = len(res.Series) - maxSeries
		res.Series = res.Series[:maxSeries]
	}
	return res, nil
}

type response struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type sample [2]any // [unix seconds, "value"]

func decode(body []byte) ([]capability.Series, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", r.Error)
	}
	switch r.Data.ResultType {
	case "vector":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Value  sample            `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &items); err != nil {
			return nil, err
		}
		out := make([]capability.Series, 0, len(items))
		for _, it := range items {
			out = append(out, series(it.Metric, []sample{it.Value}))
		}
		return out, nil
	case "matrix":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Values []sample          `json:"values"`
		}
		if err := json.Unmarshal(r.Data.Result, &items); err != nil {
			return nil, err
		}
		out := make([]capability.Series, 0, len(items))
		for _, it := range items {
			out = append(out, series(it.Metric, it.Values))
		}
		return out, nil
	case "scalar":
		var s sample
		if err := json.Unmarshal(r.Data.Result, &s); err != nil {
			return nil, err
		}
		return []capability.Series{series(map[string]string{}, []sample{s})}, nil
	default:
		return nil, errors.New("unsupported result type " + strconv.Quote(r.Data.ResultType))
	}
}

// series converts samples, leaving out non-finite values, which JSON cannot carry.
func series(labels map[string]string, samples []sample) capability.Series {
	s := capability.Series{Labels: labels, Points: []capability.Point{}}
	var lo, hi, last, sum float64
	for _, raw := range samples {
		ts, ok1 := raw[0].(float64)
		str, ok2 := raw[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		sec, frac := math.Modf(ts)
		p := capability.Point{Time: time.Unix(int64(sec), int64(frac*1e9)).UTC(), Value: v}
		if len(s.Points) == 0 || v < lo {
			lo = v
		}
		if len(s.Points) == 0 || v > hi {
			hi = v
		}
		last, sum = v, sum+v
		s.Points = append(s.Points, p)
	}
	if n := len(s.Points); n > 0 {
		avg := sum / float64(n)
		s.Min, s.Max, s.Avg, s.Last, s.Samples = &lo, &hi, &avg, &last, n
	}
	if n := len(s.Points); n > maxPoints {
		s.PointsOmitted = n - maxPoints
		s.Points = s.Points[n-maxPoints:]
	}
	return s
}

func unix(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}
