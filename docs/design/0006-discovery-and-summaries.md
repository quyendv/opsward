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
`per-item` and, in its outcome, how many items were read and how many kept. Listings carry
no knowledge notes.

**Profiles.** No single binding chooses a profile for a whole listing, so a listing runs
once for each profile through which the call is reachable (a binding without a profile
counts as the reader's), and keeps an item only when policy allows it through the profile
it was read with. A team bound through its own profile thus lists with that profile, and
the cluster enforces that profile's boundary as for any other call (0002); what the reader
could see but the team's bindings do not grant through the reader is not shown. A profile
that cannot list (a namespaced credential asked for the cluster's namespaces) leaves its
items out without failing the others; the listing fails only when every profile does. A
deny or approval that covers a whole namespace, or every namespace (a pattern such as
`*`), hides it, as Decide would refuse it.

Both `list_namespaces` and `list_workloads` filter by the environment's namespace
patterns, and the engine checks every item against the environment again.

**Permissions.** The reader needs `list` on namespaces for `list_namespaces`, and for
`list_workloads` across an environment: in an environment spanning every namespace it
lists Deployments, StatefulSets and DaemonSets cluster-wide, which needs `list` on them
cluster-wide; in one limited to some namespaces it lists namespace by namespace, which a
RoleBinding in each namespace allows.

Asked "is anything unhealthy", an agent otherwise lists every namespace one call at a time;
`list_workloads` with `unhealthy` and no namespace answers in one call.

## Summaries

A series is returned with `min`, `max`, `avg`, `last` and `samples` (the number of points
it had). Points are returned only when the call asks for them with `points: true`, and
then as before, at most 60 per series. `diagnose_workload` returns summaries only.

Log results are bounded the same way. A line longer than 2,000 bytes is cut, with the
number of bytes left out. A `query_logs` result past 32 KiB, counted as JSON-encoded since
that is what the agent receives, stream labels included, leaves out lines oldest first and
counts them in `linesOmitted`; a stream left without lines is not returned; container logs are capped at 16 KiB per container and 32 KiB per call.
Clients cap a tool result at some 25,000 tokens, structured logs run to kilobytes per line,
and escaped JSON is dense in tokens: a result an agent cannot read whole is one it cannot
use.

## Cluster-wide queries

`namespace` becomes optional for `query_metrics` and `query_logs`. Without it, the query
is confined to the environment's cluster only, so node and cluster-level series are
reachable, and a query that names a namespace in its own matchers is accepted as
written. Such a call covers every namespace, so:

- it is refused in an environment limited to some namespaces (one without the pattern
  `*`): it would read the namespaces the environment leaves out;
- a deny or approval binding for the principal, tool and environment applies to it
  whatever its namespaces, workloads, apps or condition: it restricts some part of what
  the call reads. Deny still overrides allow, so a principal denied one namespace must
  name namespaces;
- an allow binding applies only if it covers every namespace (no namespace selector, or
  one such as `*`), has no workload or app selector, and its condition, if any, does not
  look at the target (`scope`).

The query tools mark such a call as spanning every namespace, and the backends refuse an
empty namespace without that mark, so a tool that forgot one cannot go unconfined.

Without a `clusterLabel`, a cluster-wide query reads the whole backend. That is right
for a backend that holds one cluster, and the reason a backend shared by several clusters
must set one.

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
