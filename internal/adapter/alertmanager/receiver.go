package alertmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/notify"
	"github.com/quyendv/opsward/internal/redact"
)

// Adapter names the alertmanager adapter in audit records.
const Adapter = "alertmanager"

// maxBodyBytes bounds one webhook request; Alertmanager groups alerts, but not by
// megabytes.
const maxBodyBytes = 1 << 20

// Invoker runs a tool call; engine.Engine implements it.
type Invoker interface {
	Invoke(ctx context.Context, p core.Principal, tool string, args json.RawMessage) (*engine.Result, error)
}

// Options configures a Receiver.
type Options struct {
	Invoker   Invoker
	Notifiers []notify.Notifier
	// Principal is the only subject whose alerts are accepted.
	Principal string
	// Mapping returns the current mapping and environments; a reload changes them.
	Mapping func() (Mapping, []Environment)
	Dedupe  time.Duration
	Workers int
	Queue   int
	// Timeout bounds one investigation and its notifications.
	Timeout time.Duration
	Now     func() time.Time
	Logger  *slog.Logger
	// Refused, if set, records a delivery refused for its principal: a denial, which is
	// audited like every other.
	Refused func(ctx context.Context, p core.Principal, reason string)
}

// Receiver takes Alertmanager webhooks and investigates their firing alerts.
type Receiver struct {
	opt   Options
	queue chan job

	mu   sync.Mutex
	seen map[string]time.Time // fingerprint -> when it was last investigated
}

type job struct {
	p     core.Principal
	alert alert
}

// New returns a Receiver; Run must be running for alerts to be investigated.
func New(opt Options) *Receiver {
	if opt.Workers <= 0 {
		opt.Workers = 2
	}
	if opt.Queue <= 0 {
		opt.Queue = 100
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 2 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Receiver{opt: opt, queue: make(chan job, opt.Queue), seen: map[string]time.Time{}}
}

// payload is Alertmanager's webhook body, version 4.
type payload struct {
	Version string  `json:"version"`
	Alerts  []alert `json:"alerts"`
}

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// Handle serves one webhook request from an authenticated principal.
func (r *Receiver) Handle(w http.ResponseWriter, req *http.Request, p core.Principal) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST an Alertmanager webhook", http.StatusMethodNotAllowed)
		return
	}
	if core.NormalizeSubject(p.Subject) != core.NormalizeSubject(r.opt.Principal) {
		r.opt.Logger.Warn("alerts refused: not the alerts principal", "subject", p.Subject)
		if r.opt.Refused != nil {
			p.Adapter = Adapter
			r.opt.Refused(req.Context(), p, "not alerts.principal")
		}
		http.Error(w, "this principal may not send alerts", http.StatusForbidden)
		return
	}
	var body payload
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBodyBytes)).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "not an Alertmanager webhook: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Version != "4" {
		http.Error(w, "only Alertmanager webhook version 4 is understood", http.StatusBadRequest)
		return
	}
	p.Adapter = Adapter
	var res struct {
		Queued     int `json:"queued"`
		Duplicates int `json:"duplicates,omitempty"`
		Dropped    int `json:"dropped,omitempty"`
	}
	for _, a := range body.Alerts {
		key := a.key()
		if a.Status != "firing" {
			// A resolved alert that fires again is a new occurrence.
			r.forget(key)
			continue
		}
		if !r.first(key) {
			res.Duplicates++
			continue
		}
		select {
		case r.queue <- job{p: p, alert: a}:
			res.Queued++
		default:
			// Alertmanager waits for the answer: a full queue drops rather than holds it.
			res.Dropped++
			r.forget(key)
			r.opt.Logger.Warn("alert dropped: investigation queue full", "alert", a.Labels["alertname"], "fingerprint", a.Fingerprint)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	// Alertmanager retries a delivery that failed: dropped alerts come back, and the ones
	// queued are then duplicates.
	status := http.StatusAccepted
	if res.Dropped > 0 {
		status = http.StatusServiceUnavailable
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// key identifies an alert: its fingerprint, or its labels when it has none.
func (a alert) key() string {
	if a.Fingerprint != "" {
		return a.Fingerprint
	}
	names := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		names = append(names, k)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, k := range names {
		fmt.Fprintf(h, "%s=%s\x00", k, a.Labels[k])
	}
	return "labels:" + hex.EncodeToString(h.Sum(nil))
}

// first reports whether the fingerprint was not investigated within the dedupe window,
// and marks it.
func (r *Receiver) first(fingerprint string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opt.Now()
	for f, at := range r.seen {
		if now.Sub(at) >= r.opt.Dedupe {
			delete(r.seen, f)
		}
	}
	if _, ok := r.seen[fingerprint]; ok {
		return false
	}
	r.seen[fingerprint] = now
	return true
}

func (r *Receiver) forget(fingerprint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, fingerprint)
}

// Run investigates queued alerts until ctx is done, and returns once the investigations
// under way have finished: their audit records and notifications are not cut short, and
// the caller can close the store after.
func (r *Receiver) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range r.opt.Workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-r.queue:
					r.investigate(context.WithoutCancel(ctx), j)
				}
			}
		})
	}
	wg.Wait()
}

// notifyTimeout bounds the notifications of one alert, apart from its investigation, so
// that one that ran out of time is still reported.
const notifyTimeout = 30 * time.Second

func (r *Receiver) investigate(parent context.Context, j job) {
	ctx, cancel := context.WithTimeout(parent, r.opt.Timeout)
	defer cancel()
	a := j.alert
	// Alerts carry what their rules put in labels and annotations: redacted like every
	// result.
	labels := make(map[string]string, len(a.Labels))
	for k, v := range a.Labels {
		labels[k] = redact.String(v)
	}
	n := notify.Notification{Alert: notify.Alert{
		Name: labels["alertname"], Severity: labels["severity"], Summary: redact.String(a.Annotations["summary"]),
		Fingerprint: a.Fingerprint, StartsAt: a.StartsAt, Labels: labels,
	}}
	mapping, envs := r.opt.Mapping()
	t, err := mapping.Map(a.Labels, envs)
	if err != nil {
		n.Error = redact.String(err.Error())
	} else {
		n.Workload = &notify.Workload{Environment: t.Environment, Namespace: t.Namespace, Kind: t.Kind, Name: t.Name}
		args, _ := json.Marshal(n.Workload)
		res, err := r.opt.Invoker.Invoke(ctx, j.p, "diagnose_workload", args)
		if err != nil {
			n.Error = redact.String(err.Error())
			// A failed investigation does not hold the window: the next delivery retries.
			r.forget(a.key())
		} else {
			n.Diagnosis = res.Data
		}
	}
	nctx, ncancel := context.WithTimeout(context.WithoutCancel(parent), notifyTimeout)
	defer ncancel()
	for _, nt := range r.opt.Notifiers {
		if err := nt.Notify(nctx, n); err != nil {
			r.opt.Logger.Error("alert notification failed", "alert", n.Alert.Name, "error", err)
		}
	}
}
