// Package notify sends the result of an alert investigation to where a team looks: a
// webhook or a Slack incoming webhook (docs/design/0012).
package notify

import (
	"context"
	"time"
)

// Notification is one investigated alert.
type Notification struct {
	Alert    Alert     `json:"alert"`
	Workload *Workload `json:"workload,omitempty"`
	// Diagnosis is diagnose_workload's redacted result; nil when the alert was not
	// investigated or the investigation failed, and Error says why.
	Diagnosis any    `json:"diagnosis,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Alert is what a notification repeats of the alert.
type Alert struct {
	Name        string            `json:"name"`
	Severity    string            `json:"severity,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	StartsAt    time.Time         `json:"startsAt,omitzero"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// Workload is the workload an alert was mapped to.
type Workload struct {
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
}

// Notifier sends notifications.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}
