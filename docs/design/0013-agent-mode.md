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

- The model sees the alert, the workload and the diagnosis, and the tools it may call:
  those policy allows the alerts principal outright in the alert's environment.
- **It may call only tools whose declared risk is `read`.** A tool that can write, at any
  strategy, is not offered, nor are the approval tools; agent mode never changes anything.
  Each call goes through the engine as the alerts principal, with the adapter `agent`, so
  policy decides it and the audit records it like any other call; a denied call is answered
  to the model as denied. A policy can give the agent its own role with a binding to the
  alerts principal and the condition `principal.adapter == "agent"`.
- **Every scoped call is pinned to the alert:** its environment, and its namespace for
  tools that take one, whatever the model asked; a call never carries an approval. Calls
  nobody waits on cannot become approval requests: where policy says approve, the engine
  denies them (this holds for alert investigations without the agent too).
- Results reach the model as the engine returns them: redacted, with knowledge-pack notes,
  cut to 16 KiB per call and 96 KiB in all.
- The loop stops at the model's answer; at `maxSteps` tool calls (default 8, at most 20) or
  the results budget, after which the model is asked once to answer with what it has; at
  `timeout` (default 3 minutes); or at a turn the model could not finish (`max_tokens`,
  `refusal`), which is not taken as an answer. At most `maxPerHour` investigations (default
  20) run an agent; past that, alerts get the diagnosis alone and the notification says why.
- The diagnosis is notified at once; the analysis follows in a second notification
  (`followUp`), so a page does not wait for a model. On shutdown an analysis under way
  stops, and its follow-up says so.
- The answer is the model's text, at most 2,000 bytes: what is wrong, the evidence for it,
  and what a person should do next. It is untrusted text: it is redacted, notifications
  escape it like every other value, and nothing acts on it.

Logs and events can carry text written to steer a model ("ignore your instructions...").
The model can only read, pinned to the alert's namespace and through policy, so the worst
such text can do is mislead the analysis; the notification keeps the diagnosis beside it,
and the audit has every call.

## Configuration

```yaml
agent:
  provider: anthropic            # the Messages API, or openai: chat completions, which
                                 # other providers and local servers (vLLM, Ollama) speak
  model: claude-sonnet-5-5
  apiKey: { file: /etc/opsward/anthropic-key }
  url: https://api.anthropic.com # default; a gateway or proxy that speaks the same API
  maxSteps: 8
  timeout: 3m
  maxPerHour: 20
alerts:
  agent: true                    # analyze investigated alerts with the agent
```

`apiKey` may be left out with `openai`, for a local server that takes none. `agent` and
`alerts.agent` are restart-only. The provider speaks HTTP directly: no SDK, so
the client's behaviour (timeouts, what it logs) is Opsward's. It sends assistant turns back
exactly as they came (thinking blocks carry a signature the API checks), caches the system
prompt, retries once after 429, 529 and 5xx answers as `retry-after` says, and follows no
redirect, so the key reaches only the configured host.

## Alternatives considered

- **A tool that runs the agent from an MCP client.** The client already is an agent; the
  value here is the alert path, where no person or client is present.
- **Letting the agent propose changes.** A proposal is reviewed anyway, but an agent that
  writes on its own, prompted by logs anyone can write to, is a different trust model;
  revisit after reading tools have been used this way.
