package config

import (
	"strings"
	"testing"
	"time"
)

const agentSection = `
agent:
  provider: anthropic
  model: claude-sonnet-5-5
  apiKey: { file: /etc/opsward/anthropic-key }
`

func TestAgentParsesWithDefaults(t *testing.T) {
	c, err := Parse([]byte(remote + agentSection + strings.Replace(alertsSection, "alerts:\n", "alerts:\n  agent: true\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Agent
	if a.URL != "https://api.anthropic.com" || a.MaxSteps != 8 || a.Timeout.Std() != 3*time.Minute || a.MaxPerHour == nil || *a.MaxPerHour != 20 || !c.Alerts.Agent {
		t.Fatalf("agent = %+v", a)
	}
}

func TestAgentRejects(t *testing.T) {
	tests := []struct{ name, doc, want string }{
		{"alerts.agent without agent", remote + strings.Replace(alertsSection, "alerts:\n", "alerts:\n  agent: true\n", 1), "alerts.agent needs the agent section"},
		{"provider", remote + strings.Replace(agentSection, "anthropic", "acme", 1), `agent.provider: "acme" is not available`},
		{"no model", remote + strings.Replace(agentSection, "  model: claude-sonnet-5-5\n", "", 1), "agent.model is required"},
		{"inline key", remote + strings.Replace(agentSection, "{ file: /etc/opsward/anthropic-key }", "sk-x", 1), "an inline value is not accepted"},
		{"no key", remote + strings.Replace(agentSection, "  apiKey: { file: /etc/opsward/anthropic-key }\n", "", 1), "agent.apiKey { file } or { env } is required"},
		{"steps", remote + agentSection + "  maxSteps: 100\n", "agent.maxSteps must be between 1 and 50"},
		{"url credentials", remote + agentSection + "  url: https://u:p@llm.example.com\n", "agent.url must be an http or https URL without credentials"},
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

func TestAgentIsRestartOnly(t *testing.T) {
	a, _ := Parse([]byte(remote + agentSection))
	b, _ := Parse([]byte(remote + agentSection + "  maxSteps: 4\n"))
	if got := a.RestartRequired(b); len(got) != 1 || got[0] != "agent" {
		t.Fatalf("RestartRequired = %v", got)
	}
}
