# 0006: Discovery, summaries and per-cluster backends

- Status: accepted
- Date: 2026-10-16
- Extends: [0002](0002-identity-policy-storage.md), [0004](0004-metrics-logs-diagnosis.md)

## Summary

An agent given only Opsward's tools and the task "check the current metrics of
environment X" could not start: nothing named the environments, their namespaces or their
workloads, so it asked the user or guessed names from elsewhere. Once it could query, a
range query returned every point of every series, when the agent needed an overview.

| Change | Tools |
|---|---|
| Discovery, filtered per item by policy | `list_environments`, `list_namespaces`, `list_workloads` |
| Summaries by default, points on request | `query_metrics`, `get_workload_metrics`, `diagnose_workload` |
| Cluster-wide queries where policy allows | `query_metrics`, `query_logs` |
| A metrics and logs backend per cluster | configuration |

## Listing tools

| Tool | Arguments | Items |
|---|---|---|
| `list_environments` | none | name, cluster, namespace patterns, whether metrics and logs are available |
| `list_namespaces` | `environment` | namespaces of the cluster that match the environment's patterns |
| `list_workloads` | `environment`, optional `namespace`, `unhealthy` | Deployments, StatefulSets and DaemonSets: namespace, kind, name, desired and ready replicas; without a namespace, across the environment's namespaces; with `unhealthy`, only those with fewer ready replicas than desired |

A listing cannot be authorized as one call: whether a scoped principal may see
namespace `team-a-api` or workload `api` depends on the item. Listing tools are therefore
authorized **per item**, in two steps:

1. **Before anything is read**, the call must be *reachable*: some `allow` binding has
   subjects that match the principal, a role that contains the tool, and a scope whose
   environment and namespace patterns match the call's. Selectors the call does not name,
   such as `workloads` and `apps` on a call without a workload, are not consulted, and
   neither is `when`. An unreachable call is denied and audited like any other, so an
   unauthorized caller costs no backend request.
2. **Each item** of the result is kept only if policy allows the tool on the item's scope:
   - an item that names a workload is decided like a call on that workload, labels
     included, since the listing read them;
   - an item without a workload (an environment, a namespace) is kept when it is reachable
     as above and no `deny` binding without `when` or narrower selectors covers it.

Items that are not kept are left out without a trace, so the result does not tell a
principal what exists beyond its reach. The audit record carries the decision rule
`per-item`. Listings read the backend with the environment's reader profile, because no
single binding chooses a profile for the whole result.

`list_namespaces` also filters by the environment's namespace patterns, and reports
patterns without wildcards that match no namespace of the cluster, which usually means a
typing mistake in the configuration.

Asked "is anything unhealthy", an agent otherwise lists every namespace one call at a time;
`list_workloads` with `unhealthy` and no namespace answers in one call.

## Summaries

A series is returned with `min`, `max`, `avg`, `last` and `samples` (the number of points
it had). Points are returned only when the call asks for them with `points: true`, and
then as before, at most 60 per series. `diagnose_workload` returns summaries only.

Log results are bounded the same way: a line longer than 2,000 characters is cut, with the
number of characters left out, and a result past 48 KiB leaves out lines oldest first and
counts them in `linesOmitted`. Structured logs run to kilobytes per line, and a result an
agent cannot read whole is a result it cannot use.

## Cluster-wide queries

`namespace` becomes optional for `query_metrics` and `query_logs`. Without it, the query
is confined to the environment's cluster only, so node and cluster-level series are
reachable. Such a call:

- is refused in an environment limited to some namespaces (one without the pattern `*`):
  it would read the namespaces the environment leaves out;
- matches only bindings whose scope does not restrict namespaces, or restricts them to
  `*`, because a call without a namespace never matches a narrower pattern.

A query that names a namespace in its own matchers is then accepted as written.

## Backends per cluster

`providers.metrics` and `providers.logs` serve every cluster: the layout of a central
store that several clusters write to, told apart by a cluster label. A cluster can name its
own backend instead, the common layout when each cluster runs its own Prometheus and Loki:

```yaml
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles: { reader: { kubeconfig: /etc/opsward/cluster-a-reader } }
        metrics: { type: prometheus, url: https://prometheus.cluster-a.example.com }
        logs: { type: loki, url: https://loki.cluster-a.example.com }
      cluster-b:
        profiles: { reader: { kubeconfig: /etc/opsward/cluster-b-reader } }
  metrics:                       # shared: clusters without their own backend
    type: prometheus
    url: https://metrics.example.com
    clusterLabel: cluster
```

A cluster's own backend takes the same fields as the shared one. `clusterLabel` is
needed only where one backend serves more than one cluster, so a shared backend used by
several clusters must set it and a cluster's own backend usually does not. An environment
whose cluster has neither its own nor a shared backend has no metrics: the tools say so for
that environment. The metrics and logs tools are registered when any backend is configured,
and backends, like the shared ones in 0004, change only with a restart.

## Alternatives considered

- **Authorize a listing as a call on the environment or namespace.** Simple, but a team
  allowed only some workloads could never list them, and a principal allowed one namespace
  could not learn its name from Opsward.
- **Return every item and let later calls be denied.** Leaks the names of every
  namespace and workload to anyone who may list at all.
- **A separate `list` verb in roles.** More precise, but one more concept in every policy;
  reachability gives the same result with the roles people already write.
