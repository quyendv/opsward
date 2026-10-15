// Package loki implements capability.Logs on the Loki HTTP API.
package loki

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/confine"
	"github.com/quyendv/opsward/internal/provider/httpapi"
)

// Options configures the backend.
type Options struct {
	HTTP           httpapi.Options
	NamespaceLabel string
	ClusterLabel   string // empty: one cluster, no cluster matcher
}

// Source hands out Logs per environment. Every query is confined before it is sent.
type Source struct {
	client *httpapi.Client
	opt    Options

	mu       sync.RWMutex
	clusters map[string]string
}

var _ capability.LogsSource = (*Source)(nil)

// NewSource builds the source; clusters maps each environment to its cluster label value.
func NewSource(opt Options, clusters map[string]string) (*Source, error) {
	c, err := httpapi.New(opt.HTTP)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	return &Source{client: c, opt: opt, clusters: clusters}, nil
}

// Update replaces the environment to cluster value map, as on a configuration reload.
func (s *Source) Update(clusters map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters = clusters
}

// Logs implements capability.LogsSource.
func (s *Source) Logs(environment string) (capability.Logs, error) {
	s.mu.RLock()
	cluster, ok := s.clusters[environment]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("logs: unknown environment %q", environment)
	}
	return &logs{s: s, cluster: cluster}, nil
}

type logs struct {
	s       *Source
	cluster string
}

func (l *logs) Query(ctx context.Context, namespace string, q capability.LogsQuery) (*capability.LogsResult, error) {
	confined, err := confine.LogQL(q.Query, confine.Scope{
		NamespaceLabel: l.s.opt.NamespaceLabel, Namespace: namespace,
		ClusterLabel: l.s.opt.ClusterLabel, Cluster: l.cluster,
	})
	if err != nil {
		return nil, err
	}
	end := q.End
	if end.IsZero() {
		end = time.Now()
	}
	params := url.Values{
		"query":     {confined},
		"start":     {strconv.FormatInt(end.Add(-q.Since).UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(q.Limit)},
		"direction": {"backward"},
	}
	body, err := l.s.client.Get(ctx, "/loki/api/v1/query_range", params)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	streams, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	return &capability.LogsResult{Query: confined, Streams: streams}, nil
}

func decode(body []byte) ([]capability.LogStream, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"` // [unix nanoseconds, line], newest first
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", r.Error)
	}
	if r.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected result type %q", r.Data.ResultType)
	}
	type stream struct {
		capability.LogStream
		newest int64
	}
	streams := make([]stream, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		st := stream{LogStream: capability.LogStream{Labels: s.Stream, Lines: make([]string, 0, len(s.Values))}}
		for _, v := range slices.Backward(s.Values) {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			st.newest = max(st.newest, ns)
			st.Lines = append(st.Lines, time.Unix(0, ns).UTC().Format(time.RFC3339Nano)+" "+v[1])
		}
		streams = append(streams, st)
	}
	// Most recent activity first; within a stream, oldest first, so that multi-line
	// content stays in order.
	slices.SortStableFunc(streams, func(a, b stream) int { return cmp.Compare(b.newest, a.newest) })
	out := make([]capability.LogStream, len(streams))
	for i, s := range streams {
		out[i] = s.LogStream
	}
	return out, nil
}
