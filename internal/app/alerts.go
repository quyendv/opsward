package app

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/quyendv/opsward/internal/adapter/alertmanager"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/notify"
)

// alertEnvironments is what alert mapping matches environments against; a reload
// replaces it.
type alertEnvironments struct {
	p atomic.Pointer[[]alertmanager.Environment]
}

func alertEnvs(cfg *config.Config) []alertmanager.Environment {
	out := make([]alertmanager.Environment, 0, len(cfg.Environments))
	for _, e := range cfg.Environments {
		out = append(out, alertmanager.Environment{Name: e.Name, ClusterLabelValue: e.ClusterLabelValue, Namespaces: e.Namespaces})
	}
	return out
}

// openAlerts builds the alert receiver (docs/design/0012), or nil without alerts.
func openAlerts(cfg *config.Config, inv alertmanager.Invoker, envs *alertEnvironments, log *slog.Logger) (*alertmanager.Receiver, error) {
	a := cfg.Alerts
	if a == nil {
		return nil, nil
	}
	mapping := alertmanager.Mapping{EnvironmentLabel: a.EnvironmentLabel, NamespaceLabel: a.NamespaceLabel}
	for _, wl := range a.WorkloadLabels {
		mapping.WorkloadLabels = append(mapping.WorkloadLabels, alertmanager.WorkloadLabel{Label: wl.Label, Kind: wl.Kind})
	}
	var notifiers []notify.Notifier
	for i, n := range a.Notify {
		field := fmt.Sprintf("alerts.notify[%d]", i)
		switch n.Type {
		case "webhook":
			h := &notify.Webhook{URL: n.URL}
			if ref := n.Token; ref != nil {
				h.Token = func() (string, error) { return ref.Resolve(field + ".token") }
			}
			notifiers = append(notifiers, h)
		case "slack":
			ref := n.WebhookURL
			notifiers = append(notifiers, &notify.Slack{URL: func() (string, error) { return ref.Resolve(field + ".webhookURL") }})
		default:
			return nil, fmt.Errorf("%s: unknown type %q", field, n.Type)
		}
	}
	return alertmanager.New(alertmanager.Options{
		Invoker: inv, Notifiers: notifiers, Principal: a.Principal, Dedupe: a.Dedupe.Std(), Logger: log,
		Mapping: func() (alertmanager.Mapping, []alertmanager.Environment) {
			if p := envs.p.Load(); p != nil {
				return mapping, *p
			}
			return mapping, nil
		},
	}), nil
}
