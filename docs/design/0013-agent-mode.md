# 0013: Agent mode

- Status: accepted
- Date: 2026-10-16
- Extends: [0001](0001-architecture.md) ("running the agent loop"), [0012](0012-alert-investigation.md)

## Summary

`diagnose_workload` is a fixed first look. What comes next depends on what it shows: the
logs of a dependency, a metric over a longer window, a runbook from a knowledge pack, the
delivery status after a deploy. Until now that next step needed a person, or an agent the
person runs. Agent mode lets Opsward run that loop itself for an alert: a model reads the
alert and the diagnosis, calls Opsward's read tools to look further, and writes a short
analysis that goes out with the notification.

It is off unless configured. Turning it on sends operational data, redacted as every result
is, to the model provider the operator names; the operator decides whether that is
acceptable for their data.

## The loop

- The model sees the alert, the workload and the diagnosis, and the tools it may call.
- **It may call only tools whose declared risk is `read`.** A tool that can write, at any
  strategy, is not offered; agent mode never changes anything. Each call goes through the
  engine as the alerts principal, with the adapter `agent`, so policy decides it and the
  audit records it like any other call; a denied call is answered to the model as denied.
- Results reach the model as the engine returns them: redacted, and cut to 16 KiB per call.
- The loop stops at the model's answer, or at `maxSteps` tool calls (default 8), or at
  `timeout` (default 3 minutes); then the model is asked once more to answer with what it
  has. At most `maxPerHour` investigations (default 20) run an agent; past that, alerts get
  the diagnosis alone and the notification says why.
- The answer is the model's text, at most 2,000 characters: what is wrong, the evidence for
  it, and what a person should do next. It is untrusted text: notifications escape it like
  every other value, and nothing acts on it.

Logs and events can carry text written to steer a model ("ignore your instructions...").
The model can only read, through policy, so the worst such text can do is mislead the
analysis; the notification keeps the diagnosis beside it, and the audit has every call.

## Configuration

```yaml
agent:
  provider: anthropic            # the Messages API; openai (chat completions) is planned
  model: claude-sonnet-5-5
  apiKey: { file: /etc/opsward/anthropic-key }
  url: https://api.anthropic.com # default; a gateway or proxy that speaks the same API
  maxSteps: 8
  timeout: 3m
  maxPerHour: 20
alerts:
  agent: true                    # analyze investigated alerts with the agent
```

`agent` and `alerts.agent` are restart-only. The provider speaks HTTP directly: no SDK, so
the client's behaviour (timeouts, what it logs) is Opsward's.

## Alternatives considered

- **A tool that runs the agent from an MCP client.** The client already is an agent; the
  value here is the alert path, where no person or client is present.
- **Letting the agent propose changes.** A proposal is reviewed anyway, but an agent that
  writes on its own, prompted by logs anyone can write to, is a different trust model;
  revisit after reading tools have been used this way.
