# 0009: Rendering backend permissions

- Status: accepted
- Date: 2026-10-16
- Extends: [0002](0002-identity-policy-storage.md) (credential profiles)

## Summary

The backend's own authorization is the real boundary; Opsward's policy is a second layer
(AGENTS.md, invariant 8). That only holds if each profile's credential can do what the
tools using it need and nothing more, and today an operator works that out by reading
code. `opsward rbac render --config FILE` prints the Kubernetes RBAC each profile needs,
derived from the configuration: which tools policy lets run through which profile, in
which environments. Opsward still never applies it: the operator reviews and applies the
output with their own credentials.

## What a tool needs

Each tool declares the backend operations it performs (`engine.Tool.Needs`), named by
capability, not by backend: `workloads.get`, `workloads.events`, `workloads.logs`,
`workloads.pods`, `workloads.list`, `namespaces.list`, `workloads.restart`,
`workloads.scale`, `delivery.applications.get`. The Kubernetes provider maps each
operation to RBAC rules. Two tests keep the declarations honest: each tool is run against
a recording fake and may only call operations it declares, and each operation is run
against a fake cluster and may only make requests its rules allow.

## Which profile needs what

For each binding that allows or approves (deny bindings grant nothing), every tool in its
role runs through the binding's profile, or through `reader` for read tools and `operator`
for the others, in each environment the binding's scope selects. A tool whose calls may
run at a lower risk declares it (`engine.Tool.Risks`: `propose_change` is a read under
`suggest`), the engine lowers a call only to a declared risk, and each declared risk is
counted here. Besides that:

- the `reader` profile reads every targeted workload before authorization, to match
  labels, so it gets `workloads.get` wherever a workload tool is reachable;
- the delivery profile (0008) gets `delivery.applications.get` in the Argo CD namespace
  of the delivery cluster. Applications in other namespaces (`<namespace>_<name>`
  tracking ids) need the same permission there, which is not rendered.

`list_workloads` without a namespace lists an environment's literal namespaces one by one,
and only lists the cluster's namespaces to match patterns; it declares
`namespaces.list` for the latter.

## Output

Per cluster, per profile, named `opsward:<profile>` with a `:cluster` or `:delivery`
suffix. Profile names are DNS-1123 labels, which have no colon, so two profiles cannot
render the same name; the renderer refuses duplicates as well, since `kubectl apply` would
merge them into one object.

| Object | Holds | Bound |
|---|---|---|
| ClusterRole `opsward:<profile>` | namespaced rules | by a RoleBinding in each namespace when every environment it serves on the cluster lists literal namespaces; otherwise by a ClusterRoleBinding |
| ClusterRole `opsward:<profile>:cluster` | cluster-scoped rules (`namespaces.list`) | ClusterRoleBinding |
| Role `opsward:<profile>:delivery` | `delivery.applications.get` | RoleBinding in the Argo CD namespace |

The subject is the ServiceAccount `opsward-<profile>` in the namespace given by
`--service-account-namespace` (default `opsward`), or what `--subject
<profile>=<User|Group|ServiceAccount>:<name>` names (`<namespace>/<name>` for a
ServiceAccount); a `--subject` for a profile that renders nothing is an error.

Policy narrower than an environment (a binding limited to some namespaces) does not narrow
the rendered RBAC: a profile is shared by every binding that uses it. Narrower RBAC needs
separate profiles.

## Alternatives considered

- **Applying the RBAC.** Opsward would need permission to grant permissions, which is
  the credential it must never hold.
- **Impersonation per principal** instead of profiles, so the cluster's RBAC decides per
  person. Deferred (0001, "later"): it needs a credential that may impersonate, and RBAC
  for every person.
