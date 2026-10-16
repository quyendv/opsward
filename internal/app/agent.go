package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/quyendv/opsward/internal/agent"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/llm"
	"github.com/quyendv/opsward/internal/notify"
)

// maxPromptDiagnosis bounds the diagnosis given to the model.
const maxPromptDiagnosis = 24 << 10

// hourly allows at most max events per clock hour window.
type hourly struct {
	max int
	now func() time.Time

	mu    sync.Mutex
	start time.Time
	count int
}

func (h *hourly) take() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if now.Sub(h.start) >= time.Hour {
		h.start, h.count = now, 0
	}
	if h.count >= h.max {
		return false
	}
	h.count++
	return true
}

// openAnalyzer builds agent mode's analysis of diagnosed alerts (docs/design/0013), or
// nil when alerts.agent is off.
func openAnalyzer(cfg *config.Config, inv agent.Invoker, tools []engine.Tool, log *slog.Logger) func(context.Context, core.Principal, notify.Notification) (string, string) {
	if cfg.Alerts == nil || !cfg.Alerts.Agent || cfg.Agent == nil {
		return nil
	}
	c := cfg.Agent
	key := c.APIKey
	model := llm.NewAnthropic(llm.AnthropicOptions{
		URL: c.URL, Model: c.Model, APIKey: func() (string, error) { return key.Resolve("agent.apiKey") },
	})
	ag := agent.New(agent.Options{Model: model, Invoker: inv, Tools: tools, MaxSteps: c.MaxSteps, Timeout: c.Timeout.Std(), Logger: log})
	limit := &hourly{max: *c.MaxPerHour, now: time.Now}
	return func(ctx context.Context, p core.Principal, n notify.Notification) (string, string) {
		if !limit.take() {
			return "", fmt.Sprintf("the agent's limit of %d investigations an hour is reached", *c.MaxPerHour)
		}
		a, err := ag.Investigate(ctx, p, prompt(n))
		if err != nil {
			return "", "the agent failed: " + err.Error()
		}
		log.Info("agent analysis", "alert", n.Alert.Name, "steps", a.Steps, "stopped", a.Stopped,
			"input_tokens", a.Usage.InputTokens, "output_tokens", a.Usage.OutputTokens)
		if a.Text == "" {
			return "", "the agent stopped without an answer (" + a.Stopped + ")"
		}
		if a.Stopped != "" {
			return a.Text + " (stopped: " + a.Stopped + ")", ""
		}
		return a.Text, ""
	}
}

// prompt is the alert, the workload and its diagnosis, as the model first sees them.
func prompt(n notify.Notification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Alert %s", n.Alert.Name)
	if n.Alert.Severity != "" {
		fmt.Fprintf(&b, " (severity %s)", n.Alert.Severity)
	}
	if n.Alert.Summary != "" {
		fmt.Fprintf(&b, ": %s", n.Alert.Summary)
	}
	if w := n.Workload; w != nil {
		fmt.Fprintf(&b, "\nWorkload: %s %s/%s in environment %s.", w.Kind, w.Namespace, w.Name, w.Environment)
	}
	d, _ := json.Marshal(n.Diagnosis)
	diag := string(d)
	if len(diag) > maxPromptDiagnosis {
		diag = strings.ToValidUTF8(diag[:maxPromptDiagnosis], "") + " … (truncated)"
	}
	fmt.Fprintf(&b, "\nDiagnosis (diagnose_workload):\n%s", diag)
	return b.String()
}
