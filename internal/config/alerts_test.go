package config

import (
	"strings"
	"testing"
	"time"
)

const alertsSection = `
alerts:
  principal: alertmanager
  environmentLabel: cluster
  notify:
    - { type: webhook, url: https://hooks.example.com/opsward, token: { file: /etc/opsward/hook-token } }
    - { type: slack, webhookURL: { file: /etc/opsward/slack-url } }
`

func TestAlertsParseWithDefaults(t *testing.T) {
	c, err := Parse([]byte(remote + alertsSection))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Alerts
	if a.NamespaceLabel != "namespace" || a.Dedupe.Std() != 30*time.Minute || len(a.WorkloadLabels) != 3 ||
		a.WorkloadLabels[0] != (AlertWorkloadLabel{Label: "deployment", Kind: "Deployment"}) || len(a.Notify) != 2 {
		t.Fatalf("alerts = %+v", a)
	}
}

func TestAlertsReject(t *testing.T) {
	tests := []struct{ name, doc, want string }{
		{"local mode", minimal + alertsSection, "alerts need identity.mode remote"},
		{"no principal", remote + strings.Replace(alertsSection, "  principal: alertmanager\n", "", 1), "alerts.principal is required"},
		{"unknown notifier", remote + strings.Replace(alertsSection, "type: webhook", "type: email", 1), `alerts.notify[0]: type "email"`},
		{"webhook without url", remote + strings.Replace(alertsSection, " url: https://hooks.example.com/opsward,", "", 1), "alerts.notify[0]: url must be an http or https URL"},
		{"slack inline url", remote + strings.Replace(alertsSection, "{ file: /etc/opsward/slack-url }", "https://hooks.slack.example.com/x", 1), "an inline value is not accepted"},
		{"slack without url", remote + strings.Replace(alertsSection, ", webhookURL: { file: /etc/opsward/slack-url }", "", 1), "alerts.notify[1]: webhookURL { file } or { env } is required"},
		{"no workload labels", remote + alertsSection + "  workloadLabels: []\n", "alerts.workloadLabels: empty"},
		{"unknown kind", remote + alertsSection + "  workloadLabels: [{ label: job, kind: Job }]\n", `alerts.workloadLabels[0]: kind "Job"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAlertsAreRestartOnly(t *testing.T) {
	a, _ := Parse([]byte(remote + alertsSection))
	b, _ := Parse([]byte(remote + strings.Replace(alertsSection, "30m", "1h", 1) + "  dedupe: 1h\n"))
	if got := a.RestartRequired(b); len(got) != 1 || got[0] != "alerts" {
		t.Fatalf("RestartRequired = %v", got)
	}
}

func TestAlertWebhookQueryWarns(t *testing.T) {
	c, err := Parse([]byte(remote + strings.Replace(alertsSection, "https://hooks.example.com/opsward", `"https://hooks.example.com/opsward?token=x"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "alerts.notify[0]: url has a query") || strings.Contains(w, "token=x") {
		t.Fatalf("warnings:\n%s", w)
	}
}
