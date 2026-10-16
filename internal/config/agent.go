package config

import (
	"net/url"
	"strings"
	"time"
)

// Agent configures agent mode (docs/design/0013); restart-only.
type Agent struct {
	Provider   string     `yaml:"provider"` // anthropic
	Model      string     `yaml:"model"`
	APIKey     *SecretRef `yaml:"apiKey"`
	URL        string     `yaml:"url"`        // default per provider
	MaxSteps   int        `yaml:"maxSteps"`   // default 8
	Timeout    *Duration  `yaml:"timeout"`    // default 3m
	MaxPerHour *int       `yaml:"maxPerHour"` // default 20; 0 runs none
}

func (c *Config) agentDefaults() {
	a := c.Agent
	if a == nil {
		return
	}
	if a.URL == "" && a.Provider == "anthropic" {
		a.URL = "https://api.anthropic.com"
	}
	if a.MaxSteps == 0 {
		a.MaxSteps = 8
	}
	if a.Timeout == nil {
		d := Duration(3 * time.Minute)
		a.Timeout = &d
	}
	if a.MaxPerHour == nil {
		n := 20
		a.MaxPerHour = &n
	}
}

func (c *Config) validateAgent(fail func(string, ...any)) {
	if c.Alerts != nil && c.Alerts.Agent && c.Agent == nil {
		fail("alerts.agent needs the agent section: which model, with which key")
	}
	a := c.Agent
	if a == nil {
		return
	}
	if a.Provider != "anthropic" {
		fail("agent.provider: %q is not available; use anthropic", a.Provider)
	}
	if a.Model == "" {
		fail("agent.model is required")
	}
	if a.APIKey == nil || (a.APIKey.File == "" && a.APIKey.Env == "") {
		fail("agent.apiKey { file } or { env } is required")
	}
	if u, err := url.Parse(a.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		fail("agent.url must be an http or https URL without credentials")
	}
	if a.MaxSteps < 1 || a.MaxSteps > 20 {
		fail("agent.maxSteps must be between 1 and 20")
	}
	if d := a.Timeout.Std(); d <= 0 || d > 10*time.Minute {
		fail("agent.timeout must be positive and at most 10m")
	}
	if *a.MaxPerHour < 0 {
		fail("agent.maxPerHour must not be negative")
	}
}

// agentWarnings describes agent settings that are valid but probably not what was meant.
func (c *Config) agentWarnings() []string {
	a := c.Agent
	if a == nil {
		return nil
	}
	var out []string
	if c.Alerts == nil || !c.Alerts.Agent {
		out = append(out, "agent is configured but alerts.agent is off: no investigation uses it")
	}
	if strings.HasPrefix(a.URL, "http://") {
		out = append(out, "agent.url is plain http: the API key crosses the network in clear")
	}
	if a.MaxPerHour != nil && *a.MaxPerHour == 0 {
		out = append(out, "agent.maxPerHour is 0: no investigation runs the agent")
	}
	return out
}
