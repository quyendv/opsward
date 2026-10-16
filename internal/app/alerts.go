package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/quyendv/opsward/internal/adapter/alertmanager"
	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/notify"
	"github.com/quyendv/opsward/internal/policy"
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
func openAlerts(cfg *config.Config, inv alertmanager.Invoker, aud engine.Auditor, envs *alertEnvironments, log *slog.Logger) (*alertmanager.Receiver, error) {
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
		Refused: func(ctx context.Context, p core.Principal, reason string) {
			rec := audit.Record{
				Phase: audit.PhaseOutcome, Principal: p,
				Decision: &core.Decision{Effect: core.EffectDeny, Rule: "alerts.principal"},
				Outcome:  &audit.Outcome{Status: audit.StatusDenied, Error: reason},
			}
			if _, err := aud.Write(context.WithoutCancel(ctx), rec); err != nil {
				log.Error("audit write failed", "what", "refused alert delivery", "error", err)
			}
		},
		Mapping: func() (alertmanager.Mapping, []alertmanager.Environment) {
			if p := envs.p.Load(); p != nil {
				return mapping, *p
			}
			return mapping, nil
		},
	}), nil
}

// alertPolicyWarnings warns when policy lets the alerts principal diagnose in no
// environment: every investigation would then be denied.
func alertPolicyWarnings(cfg *config.Config, pol *policy.Policy) []string {
	a := cfg.Alerts
	if a == nil {
		return nil
	}
	p := core.Principal{Subject: core.NormalizeSubject(a.Principal)}
	for g, members := range cfg.Identity.Groups {
		for _, m := range members {
			if core.NormalizeSubject(m) == p.Subject {
				p.Groups = append(p.Groups, g)
			}
		}
	}
	for _, e := range cfg.Environments {
		req := core.Request{Principal: p, Tool: "diagnose_workload", Risk: core.RiskRead, Scope: core.Scope{Environment: e.Name}}
		if pol.Decide(req).Effect == core.EffectAllow {
			return nil
		}
	}
	return []string{fmt.Sprintf("alerts: no binding lets %s run diagnose_workload, so every investigation is denied (unless its token carries groups that are allowed)", a.Principal)}
}
