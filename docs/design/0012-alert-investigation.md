# 0012: Alert investigation

- Status: accepted
- Date: 2026-10-16
- Extends: [0001](0001-architecture.md) (webhook ingress), [0004](0004-metrics-logs-diagnosis.md), [0005](0005-remote-access.md)

## Summary

When an alert fires, the first minutes go to the same reads: the workload's status, its
events, the crashed container's last lines, its CPU and memory. `diagnose_workload` already
does them in one call. This note has Alertmanager hand alerts to Opsward, which maps each
firing alert to a workload, runs the diagnosis through the engine, and sends the result to
where the team looks. No model is called: the answer arrives with the alert, under the same
policy and audit as any other call. An agent mode that investigates further builds on this
later.

## Receiving alerts

`POST /alerts/alertmanager` on the remote server takes Alertmanager's webhook payload
(version 4). It needs a bearer token like `/mcp`; Alertmanager sends one from a file
(`http_config.authorization.credentials_file`). The token's principal, typically an API
token with a subject such as `alertmanager`, is the caller of every investigation, so
policy decides what alerts may investigate: a binding allowing `diagnose_workload` to that
principal, as narrow as the operator wants. Investigations are audited like any call, with
the adapter `alertmanager`.

The request is answered at once (202) with how many alerts were queued; investigations run
in the background, a few at a time, from a bounded queue. A full queue drops the alert and
logs it rather than holding Alertmanager's request. Resolved alerts are not investigated.

## From an alert to a workload

```yaml
alerts:
  principal: alertmanager        # the only subject whose alerts are accepted
  environmentLabel: cluster      # matched against each environment's clusterLabelValue
  namespaceLabel: namespace
  workloadLabels:                # alert label -> workload kind, tried in order
    - { label: deployment, kind: Deployment }
    - { label: statefulset, kind: StatefulSet }
    - { label: daemonset, kind: DaemonSet }
  dedupe: 30m                    # an alert (fingerprint) is investigated once per window
  notify:
    - { type: webhook, url: https://hooks.example.com/opsward, token: { file: /etc/opsward/hook-token } }
    - { type: slack, webhookURL: { file: /etc/opsward/slack-webhook-url } }
```

The environment is the one whose cluster label value equals the alert's label and whose
namespace patterns cover the alert's namespace; without `environmentLabel`, any
environment whose namespaces cover it, when exactly one does. An alert that maps to no
workload, or to several environments, is not investigated; the notification says why, so
a team learns which alert rules lack the labels.

## Notifying

Each investigated alert produces one notification: the alert's name, severity, summary and
labels, the workload, and the diagnosis, redacted like every result.

- `webhook` posts JSON (`{alert, workload, diagnosis, error}`), with a bearer token from a
  file if given.
- `slack` posts a short message to an incoming webhook URL, kept in a file because the URL
  is the credential: readiness, the waiting reasons, the last termination, warning events
  and the last log lines of a crashed container.

Notification failures are logged and do not retry; the audit has the investigation.

## Alternatives considered

- **Polling Alertmanager's API** instead of receiving webhooks: one more credential and
  a schedule, for what Alertmanager already pushes.
- **Running every read tool**: the diagnosis is the bounded first look; an agent mode
  decides what to read next.
