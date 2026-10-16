package config

import (
	"fmt"
	"net/url"
	"slices"
	"time"
)

// Alerts configures alert investigation (docs/design/0012); restart-only.
type Alerts struct {
	// Principal is the subject whose alerts are accepted, and the caller of every
	// investigation.
	Principal        string               `yaml:"principal"`
	EnvironmentLabel string               `yaml:"environmentLabel"`
	NamespaceLabel   string               `yaml:"namespaceLabel"` // default namespace
	WorkloadLabels   []AlertWorkloadLabel `yaml:"workloadLabels"` // default deployment, statefulset, daemonset
	Dedupe           *Duration            `yaml:"dedupe"`         // default 30m
	Notify           []AlertNotify        `yaml:"notify"`
}

// AlertWorkloadLabel names the alert label carrying a workload of Kind.
type AlertWorkloadLabel struct {
	Label string `yaml:"label"`
	Kind  string `yaml:"kind"`
}

// AlertNotify is where an investigation is sent.
type AlertNotify struct {
	Type string `yaml:"type"` // webhook or slack
	// webhook
	URL   string     `yaml:"url"`
	Token *SecretRef `yaml:"token"`
	// slack: the incoming webhook URL is the credential
	WebhookURL *SecretRef `yaml:"webhookURL"`
}

var workloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

func (c *Config) alertsDefaults() {
	a := c.Alerts
	if a == nil {
		return
	}
	if a.NamespaceLabel == "" {
		a.NamespaceLabel = "namespace"
	}
	if a.WorkloadLabels == nil {
		a.WorkloadLabels = []AlertWorkloadLabel{
			{Label: "deployment", Kind: "Deployment"}, {Label: "statefulset", Kind: "StatefulSet"}, {Label: "daemonset", Kind: "DaemonSet"},
		}
	}
	if a.Dedupe == nil {
		d := Duration(30 * time.Minute)
		a.Dedupe = &d
	}
}

func (c *Config) validateAlerts(fail func(string, ...any)) {
	a := c.Alerts
	if a == nil {
		return
	}
	if c.Identity.Mode != ModeRemote {
		fail("alerts need identity.mode remote: Alertmanager reaches Opsward over HTTP")
	}
	if a.Principal == "" {
		fail("alerts.principal is required: the subject of the token Alertmanager sends")
	}
	if a.WorkloadLabels != nil && len(a.WorkloadLabels) == 0 {
		fail("alerts.workloadLabels: empty, so no alert would map to a workload; leave it out for the defaults")
	}
	if *a.Dedupe < 0 {
		fail("alerts.dedupe must not be negative")
	}
	for i, wl := range a.WorkloadLabels {
		if wl.Label == "" {
			fail("alerts.workloadLabels[%d]: label is required", i)
		}
		if !slices.Contains(workloadKinds, wl.Kind) {
			fail("alerts.workloadLabels[%d]: kind %q is not Deployment, StatefulSet or DaemonSet", i, wl.Kind)
		}
	}
	for i, n := range a.Notify {
		switch n.Type {
		case "webhook":
			if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fail("alerts.notify[%d]: url must be an http or https URL", i)
			} else if u.User != nil {
				fail("alerts.notify[%d]: url must not contain credentials; use token", i)
			}
			if n.Token != nil && n.Token.File == "" && n.Token.Env == "" {
				fail("alerts.notify[%d]: token needs { file } or { env }", i)
			}
		case "slack":
			if n.WebhookURL == nil || (n.WebhookURL.File == "" && n.WebhookURL.Env == "") {
				fail("alerts.notify[%d]: webhookURL { file } or { env } is required", i)
			}
		default:
			fail("alerts.notify[%d]: type %q is not available; use webhook or slack", i, n.Type)
		}
	}
}

// alertWarnings describes alert settings that are valid but probably not what was meant.
func (c *Config) alertWarnings() []string {
	if c.Alerts != nil && len(c.Alerts.Notify) == 0 {
		return []string{fmt.Sprintf("alerts: no notify target; investigations of alerts from %s are only audited", c.Alerts.Principal)}
	}
	return nil
}
