package capability

import (
	"context"
	"time"
)

// Point is one sample.
type Point struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
}

// Series is one metric series with its summary; non-finite samples are left out.
type Series struct {
	Labels map[string]string `json:"labels"`
	// The summary covers every finite sample, and is absent when there is none (only NaN
	// or Inf). Samples counts them.
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Avg     *float64 `json:"avg,omitempty"`
	Last    *float64 `json:"last,omitempty"`
	Samples int      `json:"samples"`
	// Points are the newest samples, at most 60; tools leave them out unless asked.
	Points []Point `json:"points,omitempty"`
	// PointsOmitted counts older points left out of Points.
	PointsOmitted int `json:"pointsOmitted,omitempty"`
}

// MetricsQuery is an instant query (Range false, at Time) or a range query.
type MetricsQuery struct {
	Query      string
	Range      bool
	Time       time.Time
	Start, End time.Time
	Step       time.Duration
}

// MetricsResult is the answer to a metrics query.
type MetricsResult struct {
	Query         string   `json:"query"` // as sent, after confinement
	Series        []Series `json:"series"`
	SeriesOmitted int      `json:"seriesOmitted,omitempty"`
}

// Metrics queries one environment's metrics. Implementations confine every query to the
// namespace before sending it.
type Metrics interface {
	Query(ctx context.Context, namespace string, q MetricsQuery) (*MetricsResult, error)
}

// MetricsSource hands out Metrics per environment.
type MetricsSource interface {
	Metrics(environment string) (Metrics, error)
}

// LogsQuery is a log query over the last Since, newest Limit lines.
type LogsQuery struct {
	Query string
	Since time.Duration
	Limit int
	End   time.Time
}

// LogStream is the lines of one stream (such as one container), oldest first, each
// prefixed with its RFC 3339 timestamp.
type LogStream struct {
	Labels map[string]string `json:"labels"`
	Lines  []string          `json:"lines"`
}

// LogsResult is the answer to a log query.
type LogsResult struct {
	Query   string      `json:"query"` // as sent, after confinement
	Streams []LogStream `json:"streams"`
}

// Logs queries one environment's logs. Implementations confine every query.
type Logs interface {
	Query(ctx context.Context, namespace string, q LogsQuery) (*LogsResult, error)
}

// LogsSource hands out Logs per environment.
type LogsSource interface {
	Logs(environment string) (Logs, error)
}
