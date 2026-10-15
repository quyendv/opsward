# 0004: Metrics, logs and diagnosis

- Status: accepted
- Date: 2026-10-15
- Extends: [0001](0001-architecture.md), [0002](0002-identity-policy-storage.md), [0003](0003-knowledge-and-reload.md)

## Summary

Milestone 3 of v0.1 adds a Prometheus-compatible **metrics** provider, a Loki **logs**
provider, four tools, and the rules that keep free-form queries inside the caller's scope.

| Tool | Needs | Risk |
|---|---|---|
| `get_workload_metrics` | metrics | read |
| `query_metrics` | metrics | read |
| `query_logs` | logs | read |
| `diagnose_workload` | Kubernetes; metrics optional | read |

## Providers

```yaml
providers:
  metrics:
    type: prometheus          # any Prometheus HTTP API: Prometheus, VictoriaMetrics, Mimir, Thanos
    url: https://metrics.example.com
    namespaceLabel: namespace # default
    clusterLabel: cluster     # empty: the backend holds one cluster and no matcher is added
    tenant: ""                # sent as X-Scope-OrgID when set
    auth:
      bearerTokenFile: /etc/opsward/metrics-token
      # basic: { username: opsward, passwordFile: /etc/opsward/metrics-password }
    caFile: /etc/opsward/metrics-ca.pem
  logs:
    type: loki
    url: https://logs.example.com
    # the same fields as metrics

environments:
  - name: production
    cluster: cluster-a
    clusterLabelValue: prod-a # the value of clusterLabel for this environment; default: cluster
```

Credentials are referenced by file, never inline. One read credential per backend is
accepted for v0.1: metrics and logs are read-only and confined by query rewriting. Per-profile
backend credentials follow when a write path to these backends exists.

**Registration follows configuration.** The metrics tools are registered only when
`providers.metrics` is set, and `query_logs` only when `providers.logs` is set. Because
registration happens at start-up, adding or removing `providers.metrics` or `providers.logs`
needs a restart: both sections join identity, storage and audit as restart-only.

Every request has a 30-second timeout, and responses are read up to 16 MiB.

## Confining queries

A query is confined when every selector in it carries the call's namespace and, where the
provider sets `clusterLabel`, the environment's cluster value. Both query tools take
`environment` and `namespace` as required typed arguments. A binding narrowed to workloads or
apps never matches them, because these calls have no workload target.

**PromQL** is parsed with the Prometheus parser. Every vector selector, including those inside
range vectors, subqueries and function arguments, gets `namespace="<ns>"` and, if configured,
`cluster="<value>"`. A selector that already has a matcher on either label is accepted only
when the matcher is `="<the same value>"`. Any other matcher (`!=`, regex, another value) is
rejected rather than rewritten, so the caller learns that the query was outside its scope.
Experimental functions are not enabled.

**LogQL** is handled without a full parser, by accepting only the shape that can be confined
mechanically: a **log query**, which is one stream selector followed by an optional pipeline.

- The query must start with `{`. Metric queries over logs (`rate({…}[5m])`, `count_over_time`)
  are rejected in v0.1.
- The stream selector is parsed: `label op "value"` pairs separated by commas, where `op` is
  `=`, `!=`, `=~` or `!~` and values are double-quoted or backtick strings.
- After the selector, no `{` may appear outside a string literal, so the query cannot contain
  a second selector.
- The namespace and cluster matchers are then added to the selector, with the same conflict
  rule as for PromQL.

This replaces the v0.1 rule in 0002 that accepted free-form LogQL only when no confinement
was needed. Log queries are confined for every caller.

## Tools

**`get_workload_metrics`** returns a workload's resource use over a time range from
templates, so it needs no query language:

| `metric` | Series |
|---|---|
| `cpu` | per-pod CPU cores used (`container_cpu_usage_seconds_total`), with requests and limits when kube-state-metrics provides them |
| `memory` | per-pod working-set bytes (`container_memory_working_set_bytes`), with requests and limits |
| `restarts` | per-pod container restarts (`kube_pod_container_status_restarts_total`) |

Pods are selected by the controller's naming scheme (Deployment `<name>-<hash>-<id>`,
StatefulSet `<name>-<ordinal>`, DaemonSet `<name>-<id>`). Template queries pass through the same
confinement as free-form ones.

**`query_metrics`** runs a PromQL expression: an instant query without `range`, otherwise a
range query.

**`query_logs`** runs a confined log query, newest first.

Shared limits:

| | Default | Limit |
|---|---|---|
| range (`range`, `since`) | 1h | 24h |
| points per series | 60 | 60; the step is derived from the range |
| series returned | | 50; the rest are counted |
| log lines | 100 | 1000 |

Each series is returned with its labels, its points, and `min`, `max` and `last`, because
an agent usually needs the summary more than the points. Log lines are grouped by stream
(pod, container), oldest first within a stream, so that multi-line content stays together
and the redaction of keys spanning lines applies.

**`diagnose_workload`** runs what a person would run first, using whatever is configured:
status, recent events, logs of the containers that are failing (the previous instance for a
container that has restarted), and, when a metrics provider exists, CPU and memory over the
last hour. Each part is reported as present, or as unavailable with the reason (`metrics:
unavailable, no metrics provider configured`). A failed part does not fail the call.
Knowledge notes attach to its result like to the individual tools'.
