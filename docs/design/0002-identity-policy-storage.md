# 0002: Identity, policy and storage

- Status: accepted
- Date: 2026-10-15
- Extends: [0001](0001-architecture.md)

## Summary

Three decisions that everything after v0.1 builds on:

1. **Two separate trust hops.** Opsward authenticates people itself and reaches backends with
   profile credentials. Neither hop requires OIDC on the Kubernetes API server.
2. **Roles bound to subjects over a scope.** A scope can be as narrow as one workload, and a
   binding can carry a CEL condition.
3. **A storage interface from v0.1.** Embedded SQLite by default, Postgres for more than one
   replica. Configuration can come from a file, the database, or both; objects defined in a
   file are read-only everywhere else.

## Identity

### Two hops

```
          (1) who is calling?                 (2) what may Opsward do here?
 person ───────────────────────► Opsward ─────────────────────────────────► cluster
          OIDC, API token,                    profile credential
          or local principal                  (reader, operator, ...)
```

Hop 1 establishes a `Principal`. Hop 2 uses a credential that belongs to Opsward, chosen per
call (see [Profiles](#profiles)). The cluster does not need to know Opsward's users, trust its
identity provider, or have OIDC configured.

The consequence is that the cluster's audit log names the profile, not the person. Opsward's
audit is the record of who asked. Credential sources that carry the person through to the
backend can be added later behind the same interface (see [Credential sources](#credential-sources)).

### Authentication methods

| Method | For | Version |
|---|---|---|
| local | one user, MCP over stdio; the principal is fixed in configuration | v0.1 |
| OIDC | people, through any compliant issuer | v0.2 |
| API token | automation and CLIs | v0.2 |

**OIDC.** Opsward validates tokens from a configured issuer. The subject comes from a
configurable claim, `email` by default; a token whose `email_verified` is not `true` is
rejected when `email` is the subject claim. For remote MCP, Opsward is an OAuth resource
server and the issuer is the authorization server. Clients are registered with the issuer in
advance, or use an API token where they can send a static header. Opsward does not act as an
authorization server itself: issuing tokens and registering clients would make it part of the
identity provider.

**API tokens.** Issued by an administrator for a subject and a set of groups. Only a hash is
stored, the token is shown once, and every token has an expiry and can be revoked.

**No passwords.** Opsward does not store or verify user passwords. Doing so would make it an
identity provider, with everything that implies: reset flows, brute-force protection, MFA.
That is better left to software built for it.

### Groups

A principal's groups are the union of two sources:

- **Claim groups** from the token, under a configurable claim (`groups` by default).
- **Internal groups**, memberships that Opsward holds itself: from the configuration file in
  v0.2, and also from the database once the web console exists.

Internal groups exist because many issuers give ordinary applications no group claim. Signing
in with a consumer or workspace account typically yields an email address and nothing else;
reading the directory's groups needs administrative access. Teams in that position have two
options:

| Option | How | Suits |
|---|---|---|
| Identity broker (Dex, Keycloak, authentik, ...) | Upstream login is federated through the broker, which owns the groups and issues a `groups` claim to every application | Several tools that should share one set of groups |
| Internal groups | Opsward maps subjects to groups itself | One tool, or no broker |

The broker is the better answer when more than one tool needs the same groups, because each
tool that keeps its own membership list is one more list to update when someone joins or leaves.

Claim groups can be given a prefix (`groupsPrefix: "oidc:"`), so that a group named `admin` at
the issuer cannot be mistaken for Opsward's internal `admin`.

### Profiles

A profile is a backend credential with a fixed permission level, declared per environment. The
profile a call runs with is chosen by the binding that authorized it (`profile:`). When the
binding names none, read tools use the environment's `reader` profile and other tools use
`operator`.

A profile per team, bound in the cluster only to that team's namespaces, makes the cluster
enforce the coarse boundary even if Opsward's policy is wrong:

| Layer | Granularity | Enforced by |
|---|---|---|
| Profile | namespace | the cluster's RBAC |
| Policy | workload, app, tool, time | Opsward |

### Credential sources

Credentials are obtained through one interface:

```go
type CredentialSource interface {
    Credential(ctx context.Context, p Principal, env Environment, profile string) (Credential, error)
}
```

Only the profile source ships before v1. Two others fit the same interface and can follow
when a team needs the person's name in the cluster's own audit log:

| Source | Requires |
|---|---|
| Impersonation | a profile allowed to `impersonate`, scoped with `resourceNames` to the users and groups it may act as |
| Token passthrough | an API server that trusts the same issuer as Opsward |

### Opsward does not write RBAC

Opsward never creates ServiceAccounts, Roles or bindings in a cluster. To grant permissions to
others, a credential needs `bind` or `escalate` (or the permissions it grants), which lets
whoever holds it grant itself anything. Opsward would become the most privileged identity in
every cluster it reaches, contrary to invariant 8.

Instead, `opsward rbac render` (v0.3) prints the ServiceAccounts, Roles and bindings that the
configured profiles need, for the operator to apply the way they apply everything else.

## Policy

### Roles and bindings

A **role** names what may be done. A **binding** grants a role to subjects over a scope, with
an effect.

```yaml
policy:
  roles:
    viewer:
      risk: [read]
    operator:
      risk: [read]
      tools: [restart_workload, scale_workload]
    proposer:
      tools: [propose_change]

  bindings:
    - name: team-a-operate-staging
      subjects: { groups: [team-a] }
      role: operator
      scope:
        environments: [staging]
        namespaces: ["team-a-*"]
        workloads: [api, worker]
      profile: team-a-operator

    - name: team-a-view-checkout
      subjects: { groups: [team-a] }
      role: viewer
      scope:
        environments: [production]
        apps: [checkout]

    - name: team-a-operate-checkout-business-days
      subjects: { groups: [team-a] }
      role: operator
      scope:
        environments: [production]
        apps: [checkout]
      effect: approve
      approvers: { groups: [platform] }
      when: 'now.getDayOfWeek("UTC") in [1, 2, 3, 4, 5]'

    - name: no-writes-during-freeze
      role: operator
      scope: { environments: [production] }
      effect: deny
      when: 'now >= timestamp("2026-12-20T00:00:00Z") && now < timestamp("2027-01-05T00:00:00Z")'
```

| Field | Meaning |
|---|---|
| `role.risk`, `role.tools` | a tool is in the role if its name is listed or its risk class is listed |
| `subjects` | `users` and `groups`; omitted means every authenticated principal |
| `scope.environments`, `scope.namespaces` | glob patterns; omitted means any |
| `scope.workloads` | names of the workload the call targets |
| `scope.apps` | values of an application label on the target workload; the key is configurable, `app.kubernetes.io/part-of` by default |
| `effect` | `allow` (default), `approve` or `deny` |
| `approvers` | who may approve, for `approve` |
| `when` | a CEL condition; the binding applies only when it evaluates to `true` |
| `profile` | the credential profile for calls this binding authorizes |

### Workload-level scope

Kubernetes RBAC cannot express "the pods of Deployment `api`": pod names are generated,
`resourceNames` does not apply to `list` or `watch`, and roles cannot select by label. Opsward
can, because its tools take a workload as a typed argument and resolve the pods themselves.
`list_workload_logs(workload: api)` is authorized against `api`.

Rules that keep this fail-closed:

- A binding that restricts `workloads` or `apps` matches only calls whose scope names a
  workload. A call without one, such as a namespace-wide query, does not match it.
- `apps` is evaluated against labels that Opsward reads from the live object with its own
  reader profile during validation, on every call, without a cache: a relabelled workload must
  not keep its old access for a cache lifetime. The lookup reads the labels and nothing else,
  and it is skipped when no binding uses `apps` or `scope.labels`. The tool reads the object
  itself only after authorization, with the call's own profile, so a narrower profile's
  cluster permissions still apply. The labels are not returned to the caller.
- A failed lookup is not reported before authorization. With the labels unknown, `apps`
  bindings whose effect is `deny` or `approve` still match and `allow` ones do not, so a
  lookup failure cannot lift a restriction; an authorized call then reports the error
  itself. An unauthorized caller cannot use error messages to learn which objects exist.

### Confining queries

A scoped binding must not be widened by a query language. A query is confined when every
selector in it carries the call's namespace and, where one backend stores several clusters,
the environment's cluster label. The label names are configured per provider.

- **Typed tools first.** `get_workload_metrics`, `list_workload_logs` and similar tools build
  their queries from templates and are confined by construction. They are also easier for an
  agent to use well.
- **PromQL** in `query_metrics` is parsed and every vector selector gets the namespace and
  cluster matchers. A selector that already has a conflicting matcher is rejected rather than
  rewritten.
- **LogQL** is accepted as a log query (one stream selector and a pipeline), whose selector
  gets the same matchers; metric queries over logs are rejected. See
  [0004](0004-metrics-logs-diagnosis.md#confining-queries).

### Evaluation

1. Collect the bindings whose subjects, role and scope match the call.
2. Evaluate each binding's `when`. A binding with `effect: deny` or `effect: approve` whose
   condition fails to evaluate counts as matching; an `allow` binding whose condition fails
   to evaluate does not. Either way the stricter outcome wins, and the failure is logged.
3. `deny` overrides `approve`, which overrides `allow`. No matching binding is `deny`.

Validation at load time rejects:

- a binding with `effect: allow` whose role contains a `destructive` tool or the `destructive`
  risk class;
- a `when` that does not compile or does not have type `bool`;
- a binding that names an unknown role, profile or environment.

An approver cannot approve their own call.

### Conditions

`when` is [CEL](https://cel.dev), the expression language Kubernetes uses for admission
policies. It is type-checked when policy is loaded, cannot loop, and has a bounded evaluation
cost, so a condition cannot hang or crash the engine.

| Variable | Type | Content |
|---|---|---|
| `principal` | map | `subject`, `groups`, `adapter` |
| `tool` | map | `name`, `risk` |
| `scope` | map | `environment`, `namespace`, `kind`, `name`, `labels` |
| `args` | dynamic map | the call's validated arguments |
| `now` | timestamp | evaluation time |

`args` is dynamic because a role spans several tools with different arguments. Loading warns
when a condition refers to an argument that no tool in the binding's role has. The cost limit is
fixed at the per-call limit Kubernetes uses for admission policies. Time zones are explicit in
expressions (`now.getHours("UTC")`); loading warns about a time accessor without one, because a
silent UTC default is a common source of wrong windows.

Conditions are optional. Most bindings need none; they exist for time windows, freezes and
argument limits (`args.replicas <= 5`) that matchers cannot express.

### Sources

Policy, roles and internal groups come from one or both sources:

| Source | Edited through | Version |
|---|---|---|
| `file` | the configuration file | v0.1 |
| `database` | the API and web console | v0.3 |

Every object records its source. Objects from the file are read-only through the API and the
console, the same model as provisioning in Grafana. A team that wants every permission change
reviewed in version control disables the database source:

```yaml
policy:
  sources: [file]
```

Names are unique across sources. The API refuses to create a database object whose name the
file already defines. If the file later adds a name that exists in the database, the file
object wins, the database object is marked shadowed and ignored, the console shows it as such,
and the event is audited. Refusing the reload instead would let a console object block a file
change, such as a new deny binding.

Changes made through the API are tool calls like any other, and they are `destructive`: editing
permissions is the most direct path to escalating them, so every change needs an approver other
than its author. A team with a single administrator uses the file source, which is reviewed in
version control instead.

### Loading the file

Opsward reads one configuration file from disk and watches it. `SIGHUP` also triggers a reload.
How the file gets there is the operator's choice: a file on a laptop, a ConfigMap mounted as a
volume, a file rendered by a deployment tool. No operator or custom resource is involved.

- **Reload is atomic.** The new file is validated completely. If it is invalid, the previous
  configuration stays in force, the error is logged, and the
  `opsward_config_last_reload_successful` metric is set to `0`.
- **Start-up is strict.** An invalid file at start-up prevents start-up.
- **Mount the ConfigMap as a directory.** A ConfigMap mounted with `subPath` is never updated in
  the running pod, so changes would wait for a restart.

## Storage

### Store

State lives behind one interface with two implementations:

| Backend | Use | Notes |
|---|---|---|
| SQLite (default) | one replica | pure Go, no CGO; one file on a persistent volume |
| Postgres | more than one replica | required for high availability |

Running more than one replica on SQLite is not supported. Schema migrations are embedded in the
binary, run at start-up, and only move forward.

What the store holds, by version:

| Data | Version |
|---|---|
| audit records | v0.1 |
| API tokens (hashes), internal groups | v0.2 (file-defined groups are loaded, not stored) |
| approvals, change snapshots, database-sourced policy | v0.3 |

Credentials are never stored; configuration references them by file or environment variable.

### Audit

Every record is written to two places:

- the **store**, so the console and API can query it;
- a **stream** (stdout, stderr or a file), for the operator's log pipeline. The stream is the
  copy meant for long-term, tamper-resistant retention outside Opsward. Under MCP over stdio,
  stdout carries the protocol, so the stream must be stderr or a file there.

Each record carries a sequence number and a hash chain: `prev` is the hash of the previous
record from the same writer and `hash` covers the record including `prev`. A deleted or altered
record breaks the chain. A chain is identified by `writer` (`opsward` unless configured). Processes that share a
store and a writer ID, such as one MCP stdio server per client session, share one chain:
the store reads the chain head and appends under one write lock. Each process's stream
then holds only its own records, so the store is what `opsward audit verify` checks, and
it checks every chain it holds.

```json
{
  "id": "01J...",
  "writer": "opsward-0",
  "seq": 1042,
  "prev": "sha256:9f2c...",
  "hash": "sha256:41ab...",
  "phase": "outcome",
  "...": "the fields described in 0001"
}
```

**When a write fails.** A `read` call writes one record after it runs. A `write` or
`destructive` call also writes an `intent` record before it runs; if the intent cannot be
written to both destinations within a timeout, the call is refused. The timeout matters for
stdout, which rarely fails but can block when a log collector applies backpressure. If the
outcome record then fails, the action has already happened: the failure is logged and counted,
and the intent record still shows the attempt.
For a `read` call the result is withheld instead: output that was not audited does not
leave the engine.

**Retention.** The store keeps records for 90 days by default (`auditRetention`, `0` keeps them
forever), in every chain the store holds; long-term retention is the stream's job. Pruning
first writes a `checkpoint` record
holding the hash of the last record it removes, so the chain stays verifiable from the cut and a
prune is distinguishable from a deletion.

## Configuration

```yaml
identity:
  oidc:
    issuer: https://id.example.com
    audience: opsward
    subjectClaim: email
    groupsClaim: groups
    groupsPrefix: "oidc:"
  groups:                         # internal groups
    platform: [alice@example.com]

storage:
  type: sqlite
  path: /var/lib/opsward/opsward.db
  auditRetention: 90d
  # type: postgres
  # dsn: { file: /etc/opsward/postgres-dsn }

audit:
  stream: { type: stderr }

policy:
  sources: [file]
  appLabel: app.kubernetes.io/part-of
  roles: { ... }
  bindings: [ ... ]
```

## Consequences

- v0.1 ships the store and the policy engine, not only an audit file, so that later versions add
  tables and a policy source rather than retrofitting them.
- The cluster audit log does not name the person until an impersonation or passthrough source
  exists. Opsward's audit must be retained and correlated for that.
- Fine-grained scope is enforced by Opsward, not by the cluster. Per-team profiles keep a policy
  mistake inside namespace boundaries.
- Opsward stays out of the business of managing users and cluster permissions: it neither stores
  passwords nor writes RBAC.

## Alternatives considered

- **A ServiceAccount per user, created by Opsward.** Gives per-person names in the cluster audit
  log without OIDC on the API server, but needs a credential that can grant itself anything, and
  leaves RBAC in the cluster that no declared source owns.
- **Custom resources and an operator for policy.** Natural for teams that run everything through
  Kubernetes, but makes Kubernetes a prerequisite for running Opsward at all, and a file mounted
  from a ConfigMap already gives the same delivery path.
- **No database until the web console.** Simpler at first, but the console needs audit history,
  approvals and editable policy, and moving from files to a database after users depend on the
  file format is a migration every one of them has to make.
- **Flat allow/deny rules** (the first draft of 0001). Equivalent in power, but repeats subjects
  and scopes in every rule and is less familiar than roles and bindings to people who know
  Kubernetes RBAC.
