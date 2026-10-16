# 0007: Operate verbs and approvals

- Status: accepted
- Date: 2026-10-16
- Extends: [0001](0001-architecture.md), [0002](0002-identity-policy-storage.md)

## Summary

The first writes: two narrow verbs applied to the cluster, and the approval flow that
`effect: approve` has named since 0002. Proposing changes through a forge follows in its
own note.

| Tool | Risk | What it does |
|---|---|---|
| `restart_workload` | write | rolling restart of a Deployment, StatefulSet or DaemonSet |
| `scale_workload` | write | set the replicas of a Deployment or StatefulSet |
| `list_approvals` | read | approval requests the caller made or may decide |
| `decide_approval` | write | approve or deny a request |

## Verbs

Both verbs run with the binding's profile, `operator` by default, and change only what
they name, through a merge patch with the field manager `opsward`:

- `restart_workload` sets the pod template annotation `kubectl.kubernetes.io/restartedAt`,
  as `kubectl rollout restart` does, so the workload's own rollout strategy applies.
- `scale_workload` sets `spec.replicas` (0 to 1,000). A workload that a
  HorizontalPodAutoscaler targets is refused: the autoscaler would undo it.

**Preview.** Before anything is changed, the patch is applied as a server-side dry run.
The difference between the live object and the dry-run result, limited to the fields the
verb changes, is the call's diff: it is shown to approvers and recorded in the audit.

**Snapshot.** The previous values of those fields are recorded with the change, so the
change can be reversed with the same verb (`scale_workload` back to the previous
replicas). Whole objects are not stored: a pod template carries environment values, and a
snapshot is only as safe as the store that holds it.

**Ownership.** A change that a reconciler would revert is refused, with the reason:

| Reconciler | Recognized by |
|---|---|
| Argo CD | annotation `argocd.argoproj.io/tracking-id`, label `argocd.argoproj.io/instance` |
| Flux | labels `kustomize.toolkit.fluxcd.io/name`, `helm.toolkit.fluxcd.io/name` |
| Helm | label `app.kubernetes.io/managed-by: Helm` |

`scale_workload` is refused on a reconciled workload: its replicas are declared in the
source, and the change belongs there. `restart_workload` is accepted, because a restart is
how incidents are handled and declares nothing; its result notes the reconciler, since one
that reverts pod template annotations will roll the workload again.

The audit record carries the change:

```json
"change": { "strategy": "direct", "diff": "spec.replicas: 3 -> 5", "previous": { "spec.replicas": 3 } }
```

## Approvals

A call whose decision is `approve` is not run. It becomes an **approval request**:

1. The engine runs the tool's preview with the call's profile, stores a request (id,
   requester, tool, scope, the hash of the arguments, the diff, the approvers named by the
   binding, an expiry), and answers with the request id and the diff. The audit record has
   status `pending`.
2. An approver lists requests with `list_approvals` and decides with
   `decide_approval(id, approve | deny, reason)`. The approver must be named by the binding
   that required approval, and must not be the requester. Deciding is itself a call:
   policy must allow the tool, and it is audited.
3. The requester calls the same tool again with the same arguments and `approval: <id>`.
   The engine runs it only if the request was approved, is the caller's own, has the same
   tool and argument hash, has not expired, and has not been used; using it is a single
   atomic step, so a request runs at most once. Policy is decided again: a deny that applies
   now still wins, and the call must still resolve to `approve` under the binding that the
   approval was for.

Requests expire after an hour unless `approvals.ttl` says otherwise; expiry counts from the
request, so a slow approval leaves less time to run. An approval is bound to the exact
arguments: changing one replica count asks for a new approval.

Approvals need a second person, so they work only in remote mode. In local mode the only
principal cannot approve its own request, and a call that needs approval cannot run.

## Storage

The store gains approval requests, with the conformance suite covering both stores:
create, read, list by requester or approver, decide (only a pending request), and use
(only an approved one, at most once).

## Alternatives considered

- **Run the call when it is approved**, as the approver's action. The agent would have to
  poll for a result produced outside its session, and the approver would cause a change
  they did not ask for. Retrying with a ticket keeps the requester the one who acts.
- **Store whole objects as snapshots.** Reversal could restore anything, but the store
  would hold every environment value of every changed workload.
- **Refuse restarts of reconciled workloads too.** Consistent, but in a cluster managed
  through GitOps nothing could be restarted, and the restart a reconciler might repeat is
  harmless.
