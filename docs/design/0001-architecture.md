# 0001: Architecture

- Status: accepted
- Date: 2026-10-15

## Summary

Opsward is a server that exposes **typed operational tools** to AI agents and puts every call
through one pipeline: identify the caller, authorize the call against policy, obtain approval
when policy requires it, run it with a credential scoped to the caller's profile, redact the
output, and record an audit entry. Agents reach it through MCP; the engine itself is
protocol-agnostic.

It is strict about safety and flexible about stack. The safety pipeline cannot be switched off.
Which backends exist, and how a change reaches a cluster, are configuration.

## Goals

- Let a team share an AI-assisted operations workflow without sharing one engineer's
  credentials.
- Give every call a principal, a policy decision and an audit record.
- Work with whatever a team already runs. GitOps, a reconciler, a log store or a tracing
  backend are optional, not prerequisites.
- Default to zero write capability: a fresh install can read and suggest, nothing else.
- Make operational knowledge (runbooks, known failure modes) part of the tool surface.

## Non-goals

- **Running the agent loop.** Until v0.4 Opsward calls no LLM; clients bring their own agent.
- **Being an MCP gateway.** Routing between many third-party MCP servers is solved elsewhere.
  Opsward can sit behind such a gateway.
- **Targets other than Kubernetes before v1.** Hosts reached over SSH would be a free-form
  command channel (invariant 2); supporting them needs its own design.
- **A general Kubernetes client.** Tools are operational tasks, not one tool per API verb.

## Terminology

| Term | Meaning |
|---|---|
| Principal | Who is calling: subject, groups, and the adapter the call came through |
| Tool | A named operation an agent can invoke, with typed arguments and a risk class |
| Risk class | `read`, `write` or `destructive`, declared by the tool, never inferred |
| Capability | An abstract backend function, such as `Logs` or `Delivery` |
| Provider | An implementation of one or more capabilities for one backend, such as `loki` |
| Environment | A named target: a cluster, a namespace selector, a change strategy and policy scope |
| Change strategy | How a change reaches an environment: `suggest`, `pull-request` or `direct` |
| Profile | A backend credential with a fixed permission level, such as `reader` or `operator` |
| Knowledge pack | A directory of Markdown documents with frontmatter that tools can surface |

## Architecture

```
 clients     agent CLIs, desktop and web chat apps, IDEs, chat bots, web console, alert webhooks
                 │ MCP              │ REST              │ webhook
 ┌───────────────▼──────────────────▼───────────────────▼──────────────────────┐
 │ adapter/      the only layer that knows a protocol                           │
 ├──────────────────────────────────────────────────────────────────────────────┤
 │ engine/       identify → resolve tool → validate → authorize → [approve]     │
 │               → run → redact → audit                                         │
 ├──────────────────────────────────────────────────────────────────────────────┤
 │ tool/         the agent's vocabulary: diagnose, propose_change, restart, ... │
 │ change/       strategies: suggest | pull-request | direct                    │
 │ knowledge/    packs, matched to tools and resources                          │
 ├──────────────────────────────────────────────────────────────────────────────┤
 │ capability/   interfaces: Workloads, Logs, Metrics, Traces, Delivery,        │
 │               Forge, CI, SourceResolver                                      │
 ├──────────────────────────────────────────────────────────────────────────────┤
 │ provider/     kubernetes, argocd, flux, prometheus, loki, tempo, github,     │
 │               gitlab, gitea, jenkins, ...  (one credential per profile)      │
 └──────────────────────────────────────────────────────────────────────────────┘
```

Dependencies point downwards only. Tools depend on capability interfaces, never on a provider,
so adding or swapping a backend does not touch a tool. Package layout follows the layers:
`internal/adapter/<protocol>`, `internal/engine`, `internal/tool`, `internal/change`,
`internal/knowledge`, `internal/capability`, `internal/provider/<name>`.

## Request lifecycle

Every invocation, from every adapter, follows the same sequence. A failure at any step ends
the call and still produces an audit record.

1. **Identify.** The adapter turns transport credentials into a `Principal`. Local stdio mode
   yields a fixed principal from configuration. Remote mode validates an OIDC token. A call
   with no principal is rejected.
2. **Resolve.** Look up the tool in the registry. An unknown name is denied, not ignored.
3. **Validate.** Decode arguments into the tool's typed input and check them against its
   schema, including that scope arguments name a configured environment and an allowed
   namespace.
4. **Authorize.** `authorize(principal, tool, args)` returns `allow`, `deny` or `approve`.
   No matching rule means `deny`.
5. **Approve.** For `approve`, the call is parked until a principal who is allowed to approve
   does so, or it expires. Until v0.3 this is unavailable, so `approve` behaves as `deny`.
6. **Run.** The tool runs with the backend profile mapped to this principal and environment.
7. **Redact.** The result passes through redaction before it leaves the engine.
8. **Audit.** One record per call, written whether the call succeeded, failed or was denied.

```go
type Principal struct {
    Subject string
    Groups  []string
    Adapter string // "mcp-stdio", "mcp-http", "rest", ...
}

type Decision struct {
    Effect Effect // Allow, Deny, Approve
    Rule   string // the rule that decided, or "default-deny"
}
```

## Tools

A tool is a descriptor plus a function. The descriptor carries everything policy and the
registry need without running the tool.

```go
type Tool struct {
    Name        string            // verb_noun, named for the task, not the backend
    Description string            // what an agent reads to choose the tool
    Risk        Risk              // Read, Write, Destructive
    Requires    []capability.Kind // registered only when all are available
    Input       any               // typed argument struct; JSON Schema is derived from it
    Scope       func(input any) Scope
    Run         func(ctx context.Context, call Call) (Result, error)
}

type Scope struct {
    Environment string
    Namespace   string
    Kind, Name  string // the resource the call targets, when it has one
}
```

Rules:

- **Typed scope.** `Scope` extracts environment, namespace and resource from typed arguments,
  so policy decides without parsing free text.
- **No command channel.** No tool accepts a shell command, a kubectl argument list, or an
  arbitrary API path or verb. Read-only query languages (PromQL, LogQL, TraceQL) are allowed.
- **Secrets are not a resource.** No tool reads Secret payloads. Tools that list resources
  return Secret names and metadata only, and only where the profile allows listing them.
- **Registration follows capability.** A tool whose required capabilities are not configured
  is not registered, so agents never see a tool that cannot work.
- **Composite tools degrade.** A tool such as `diagnose_workload` uses whatever is available
  and reports each missing part explicitly (`logs: unavailable, no logs provider configured`)
  instead of failing.

Initial tool set, by group:

| Group | Tools | Risk |
|---|---|---|
| Investigate | `get_workload_status`, `list_workload_events`, `list_workload_logs`, `query_metrics`, `query_logs`, `get_trace`, `diagnose_workload` | read |
| Context | `resolve_source`, `get_change_history`, `get_drift`, `search_knowledge` | read |
| Change | `propose_change` | depends on strategy |
| Operate | `restart_workload`, `scale_workload`, `rollback_workload`, `sync_application` | write |

`propose_change` is `read` under `suggest`, because it only returns a diff, and `write` under
`pull-request` and `direct`. Its effective risk is computed from the environment's strategy
before authorization.

## Capabilities and providers

| Capability | Purpose | First providers |
|---|---|---|
| `Workloads` | read workloads, pods, events, pod logs; narrow mutations | kubernetes |
| `Logs` | centralized log queries | loki; kubernetes (pod logs) as fallback |
| `Metrics` | instant and range queries | prometheus (any Prometheus-API backend) |
| `Traces` | trace lookup and search | tempo |
| `Delivery` | sync status, desired vs live, deploy history, sync | argocd, none |
| `Forge` | branches, commits, pull requests | github, gitea |
| `CI` | pipeline status and logs | jenkins |
| `SourceResolver` | map a live object to the files that declare it | argocd, annotation, static map |

Interfaces are written for all of them, but implementations ship only when someone uses them.

Each provider is configured with one credential per **profile**. A profile has a fixed level
(`reader`, `operator`), and the backend's own authorization is the real boundary: a `reader`
Kubernetes credential is bound to a role without `secrets`, `pods/exec`, `pods/portforward`,
`nodes/proxy`, `create pods`, `escalate`, `bind` or `impersonate`. Opsward's policy is a
second layer on top, never the only one.

## Change model

How a change reaches an environment is a per-environment setting.

| Strategy | What happens | Credentials Opsward needs |
|---|---|---|
| `suggest` (default) | Return a unified diff or patch; a human applies it | none for writing |
| `pull-request` | Commit to a branch and open a pull request in the source repository | forge write |
| `direct` | Server-side apply to the cluster, with the safeguards below | cluster `operator` |

**`pull-request` does not require a reconciler.** It needs a forge and a resolvable source.
What applies the merged change (Argo CD, Flux, a CI job, a person) is outside Opsward.
A `Delivery` provider, when configured, adds sync status and history; it is not required.

**Source resolution.** `pull-request` needs to know which repository, path and file declare a
live object. Resolvers are tried in order and the first answer wins:

1. a `Delivery` provider (an Argo CD Application or Flux object that owns the resource)
2. an annotation on the object, `opsward/source: <repo>//<path>` (the key is configurable)
3. a static mapping in configuration

If nothing resolves, `propose_change` falls back to `suggest`. It never falls forward to
`direct`.

**`direct` safeguards.** Removing a pull request removes review, so `direct` rebuilds its
properties:

1. **Preview.** Every apply is preceded by a server-side dry run, and the diff is part of the
   approval request and the audit record.
2. **Ownership check.** If a reconciler or package manager manages the object (Argo CD tracking
   metadata, Flux labels, Helm ownership), a direct change would be reverted or create drift.
   The call is refused, with a pointer to `pull-request` when the source resolves.
3. **Snapshot.** The object's prior state is stored with the audit record. Rollback is a
   re-apply of that snapshot through the same pipeline.
4. **Field managers.** Applies use a dedicated field manager (`opsward`), so changes are
   attributable in the object itself.

## Policy

Policy is a list of rules. Each rule matches on principal, environment, namespace, tool and
risk, and has an effect.

```yaml
policy:
  rules:
    - name: team-a-read
      principals: { groups: [team-a] }
      namespaces: ["team-a-*"]
      risk: [read]
      effect: allow

    - name: team-a-restart-staging
      principals: { groups: [team-a] }
      environments: [staging]
      namespaces: ["team-a-*"]
      tools: [restart_workload]
      effect: allow

    - name: production-writes
      environments: [production]
      risk: [write, destructive]
      effect: approve
      approvers: { groups: [platform] }
```

Evaluation:

- `deny` overrides `approve`, which overrides `allow`. No match is `deny`.
- `destructive` can never resolve to `allow`. Configuration validation rejects a rule that
  would allow it without approval.
- An approver cannot approve their own call.
- Policy is loaded at start-up and validated completely. An invalid policy prevents start-up
  rather than loading partially.

## Identity

| Mode | Principal comes from | Version |
|---|---|---|
| local | configuration (`principal: {subject: local-admin, groups: [admin]}`) | v0.1 |
| remote | OIDC access token, validated against the issuer; groups from a claim | v0.2 |

**Gate:** remote mode, a second user, or any tool above `read` requires identity, policy and
audit to be complete. These are not features that can follow later.

Backends see a profile credential, not the human: Opsward's audit is the record of who asked.

## Audit

One JSON record per call, written to a configured sink (stdout, file; others later).

```json
{
  "id": "01J...",
  "time": "2026-01-01T00:00:00Z",
  "principal": { "subject": "alice@example.com", "groups": ["team-a"], "adapter": "mcp-http" },
  "tool": "restart_workload",
  "risk": "write",
  "scope": { "environment": "staging", "namespace": "team-a-api", "kind": "Deployment", "name": "api" },
  "args": { "...": "redacted copy of the input" },
  "decision": { "effect": "allow", "rule": "team-a-restart-staging" },
  "outcome": { "status": "ok", "durationMs": 412 },
  "change": { "strategy": "direct", "diff": "...", "snapshot": "..." }
}
```

Denied and failed calls are recorded too. An audit sink that cannot be written to fails the
call: an unaudited action is worse than a refused one.

## Redaction

Output is redacted before it leaves the engine, in two passes:

1. **Structural.** Known sensitive fields are removed by path: Secret `data` and `stringData`,
   environment variables whose value is a literal (as opposed to a reference), kubeconfig and
   token fields.
2. **Pattern.** Content that looks like a credential (cloud keys, bearer tokens, private keys,
   connection strings with passwords) is replaced, using the same rule set as the repository's
   secret scanner.

Redaction is a safety net, not the boundary. The boundary is that profiles cannot read Secrets
and tools do not ask for them.

## Knowledge packs

A pack is a directory of Markdown files with frontmatter:

```markdown
---
id: rwo-rolling-update-deadlock
title: Single-replica RWO workloads deadlock under RollingUpdate
applies-to:
  tools: [restart_workload, rollback_workload]
  match: { kind: Deployment, replicas: 1, volumeAccessMode: ReadWriteOnce }
severity: warning
---
The new pod cannot attach the volume while the old pod holds it ...
```

Packs are used in two ways:

- **Searched.** `search_knowledge` and MCP resources expose them to agents on request.
- **Attached.** When a tool's call matches a document's `applies-to`, the document's summary is
  added to the tool result as a warning. This is how hard-won lessons reach an agent that did
  not think to ask.

Opsward ships a small set of generic packs. Organization-specific packs live in the
operator's own repository and are loaded by path.

## Configuration

A single YAML file, validated completely at start-up. Credentials are never inline; each is a
reference to a file or an environment variable.

```yaml
identity:
  mode: local
  principal: { subject: local-admin, groups: [admin] }

providers:
  kubernetes:
    clusters:
      cluster-a: { profiles: { reader: { kubeconfig: /etc/opsward/cluster-a-reader } } }
  metrics: { type: prometheus, url: https://metrics.example.com }
  logs: { type: loki, url: https://logs.example.com }
  delivery: { type: none }
  forge: { type: none }

environments:
  - name: staging
    cluster: cluster-a
    namespaces: ["*"]
    change: { strategy: suggest }

knowledge:
  paths: [/etc/opsward/knowledge]

audit:
  sink: { type: file, path: /var/log/opsward/audit.jsonl }
```

## Adapters

- **MCP**, using the official Go SDK, over stdio (local) and streamable HTTP (remote). Tools map
  to MCP tools, knowledge packs to MCP resources.
- **REST** (v0.3) for the web console: approvals, audit queries.
- **Webhook ingress** (v0.4) for alert-driven investigation.

## Threat model

| Threat | Mitigation |
|---|---|
| A principal invokes a tool or reaches a resource policy does not grant | single authorization point; default deny; backend RBAC as a second boundary |
| Prompt injection through logs, events or repository content | no free-form command tool; writes need policy and, for risky ones, a human approver who sees the diff |
| Secret disclosure to a model provider | profiles cannot read Secrets; structural and pattern redaction |
| An agent combines untrusted input, private data and an exfiltration channel | Opsward cannot see the client's other tools, so it limits its own surface: no generic HTTP or fetch tool, forge writes only to configured repositories, approvals show the full diff |
| Opsward's own credentials are stolen | per-profile least privilege; no single administrative credential; credentials referenced, never inline |
| A change bypasses review through a reconciled object | ownership check refuses `direct` on reconciled objects |
| Silent gaps in audit | an unwritable sink fails the call |

## Roadmap

| Version | Scope |
|---|---|
| v0.1 | local identity, MCP stdio, read tools for Workloads, Metrics, Logs; `suggest`; knowledge packs; audit to file |
| v0.2 | OIDC, remote MCP over HTTP, policy engine, multiple principals |
| v0.3 | `pull-request` and `direct` strategies, narrow write verbs, approvals, minimal web console |
| v0.4 | Delivery provider depth, alert webhook, optional agent mode |

## Alternatives considered

- **An agent runtime such as kagent.** It runs agent loops as cluster resources and binds
  permissions to the agent, not to the human who asked. Opsward is a tool layer such a runtime
  can call; the two compose.
- **Existing Kubernetes and observability MCP servers behind a gateway.** This covers read-only
  use well and should be the recommendation for a team that needs only that. It does not
  provide per-principal backend profiles, a write path through review, ownership checks, or
  knowledge attached to tool calls.
- **Mandating GitOps.** Simpler, but excludes teams without it and does not match how many
  organizations actually run a mix of reconciled and hand-applied workloads.
