# 0010: Delivery status

- Status: accepted
- Date: 2026-10-16
- Extends: [0001](0001-architecture.md) (the `Delivery` capability), [0008](0008-propose-change.md)

## Summary

An agent diagnosing a workload sees pods, events, logs and metrics, but not what the
reconciler thinks: whether the live state matches Git, which revision is deployed, whether
the last sync failed and why. After `propose_change` opens a pull request and someone
merges it, the same question decides whether the change has landed. `get_delivery_status`
answers it for one workload, read-only, from the Argo CD Application that manages it.

## The tool

`get_delivery_status` (read) takes the workload's scope, like every workload tool, and is
authorized on it. The Application is found as in 0008: by the workload's tracking id (or
label, with `labelTracking`), which must name the workload, in the delivery namespace or
one `applicationNamespaces` lists, deploying to the workload's namespace on the
environment's cluster. The delivery cluster is Argo CD's `in-cluster`; another cluster is
reached only through the destinations `providers.delivery.destinations` lists for it
(a server URL or a cluster name), so an Application for one environment never answers for
another. With label tracking, a Helm release named like an unrelated Application in the
same namespace is taken for it: prefer the tracking-id annotation. It returns:

| Field | From the Application |
|---|---|
| `application` | namespace/name |
| `sync` | sync status, the revision live and the revision targeted (per source for a multi-source Application) |
| `health` | health status and message |
| `operation` | the last operation's phase, message, start and finish times, revision |
| `conditions` | type and message of each condition (comparison errors, sync errors), at most 10; shared resource warnings, which name other Applications, are left out |
| `resources` | the resources in the workload's namespace that are out of sync or not healthy, unhealthy first: group, kind, name, sync and health status; at most 50, with a count of the rest and of those in other namespaces or cluster-wide, which the caller is not authorized on by name |
| `history` | the last 5 deployed revisions, newest first, with when they were deployed |

Messages pass through redaction like every result and are cut at 1 KiB: a render error
can run to kilobytes. When Argo CD keeps resource health out of the Application
(`resourceHealthSource: appTree`, the default since Argo CD 3), the answer says that
resources show their sync status only. A workload no Application manages is
answered with that fact, not an error, so an agent can tell "not managed by Argo CD" from
"could not read".

## Access

The Application is read with the delivery profile on the delivery cluster, as source
resolution does (0008), and the workload's metadata with the call's profile. The tool
needs `workloads.get`; `opsward rbac render` (0009) renders the delivery profile's `get`
on Applications whenever `providers.delivery` is configured. The tool, and that RBAC,
exist only with `providers.delivery`, which no longer requires a forge.

## Alternatives considered

- **Reading Argo CD's API server** instead of the Application objects. It adds managed
  resource diffs and logs, but needs an Argo CD account and token, and the objects carry
  what an agent needs to decide.
- **A `sync_application` write.** Syncing is what Argo CD does on its own or what a person
  triggers after review; with `propose_change`, Opsward's write path stays in Git.
