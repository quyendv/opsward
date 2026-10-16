# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] - 2026-10-16

Agent mode: an alert's diagnosis is followed by a model's analysis, made with Opsward's
read tools under the same policy and audit, pinned to the alert's workload.

### Added

- Agent mode for alerts (`agent`, `alerts.agent: true`; off by default): after the
  diagnosis, a model reads the alert and the diagnosis, calls Opsward's read tools through
  the engine as the alerts principal (adapter `agent`, under policy and audit; tools that
  can write are never offered, nor tools policy would deny), and its short analysis follows
  the diagnosis in a second notification (`followUp`).
  Every scoped call is pinned to the alert's environment and namespace. Bounded by
  `maxSteps`, `timeout`, a results budget and `maxPerHour`; results reach the model
  redacted. Providers: Anthropic's Messages API, or OpenAI's chat completions, which other
  providers and local servers (vLLM, Ollama) speak; turning agent mode on sends redacted
  operational data to the provider.

### Changed

- Calls nobody waits on (alert investigations, the agent) are denied where policy says
  approve, instead of becoming approval requests.

## [0.6.0] - 2026-10-16

Running for a team: a Helm chart for the cluster Opsward serves, and alerts that arrive
already diagnosed.

### Added

- A Helm chart, `charts/opsward`, published with each release as
  `oci://ghcr.io/quyendv/charts/opsward`: Opsward in remote mode behind a Service, the
  configuration in a ConfigMap (policy and other reloadable sections apply in place; the
  rest roll the pod), SQLite on a volume (or Postgres with more replicas), and a
  ServiceAccount with a token and a kubeconfig for each profile in `localProfiles`, the
  subjects `opsward rbac render` binds; its reviewed output can ship as `rbac.rendered`.
  The pod runs as non-root with a read-only filesystem and no token of its own.
- Alert investigation: `POST /alerts/alertmanager` takes Alertmanager's webhook, maps each
  firing alert to a workload from its labels (`alerts.environmentLabel`, `namespaceLabel`,
  `workloadLabels`), runs `diagnose_workload` through the engine as `alerts.principal`, under
  policy and audit, and sends the result to a webhook (JSON) or Slack (a short summary:
  readiness, waiting reasons, warning events, the crashed container's last lines). Alerts
  are investigated once per `alerts.dedupe` window; ones that map to no workload are
  reported with the reason. No model is called.
- `opsward rbac render --cluster NAME` prints one cluster's RBAC.
- `sourceResolution.annotationPaths` bounds, per environment, the repository paths a
  workload's `opsward/source` annotation may name.
- The chart takes `env`, `envFrom`, `extraVolumes` and `extraVolumeMounts`, refuses an
  Ingress without TLS unless `ingress.allowPlainHTTP` says TLS ends in front of it, and is
  published with a provenance attestation.

## [0.5.0] - 2026-10-16

Backend permissions and delivery state: the RBAC each credential profile needs, derived from
policy, and how Argo CD sees a workload.

### Added

- `opsward rbac render --config FILE` prints the Kubernetes RBAC each profile needs for
  the tools policy lets run through it: RoleBindings where an environment lists its
  namespaces, ClusterRoleBindings where it has patterns, `list` on namespaces cluster-wide
  where `list_namespaces` or `list_workloads` is allowed, and `get` on Argo CD Applications
  for the delivery profile. Objects are named `opsward:<profile>`; subjects default to
  ServiceAccounts `opsward-<profile>` (`--service-account-namespace`, `--subject`).
  Nothing is applied.
- `get_delivery_status`: how Argo CD sees a workload, from the Application that manages
  it: sync status with the live and target revisions, health, the last sync operation and
  its error, conditions, the resources out of sync or unhealthy (unhealthy first, at most
  50; only the workload's namespace is shown by name) and the last 5 deployments.
  Registered with `providers.delivery`, which no longer needs a forge.

### Changed

- Profile names must be DNS-1123 labels (lowercase letters, digits and `-`).
- Argo CD Applications outside `providers.delivery.namespace` are read only from the
  namespaces `providers.delivery.applicationNamespaces` lists.
- An Argo CD Application manages a workload only if it deploys to the workload's
  cluster: in-cluster for the delivery cluster, or a destination
  `providers.delivery.destinations` lists for another cluster.
- `list_workloads` without a namespace, in an environment that lists its namespaces, lists
  them one by one without listing the cluster's namespaces first.

## [0.4.0] - 2026-10-16

The write path: narrow changes to workloads, previewed and audited, with approvals by
someone else where policy says so, and changes proposed as pull requests against the
GitOps source instead of made in the cluster.

### Added

- `restart_workload` and `scale_workload`: narrow changes applied with the binding's
  profile (`operator` by default) and the field manager `opsward`, each a server-side dry
  run first, with the diff and previous values recorded in the audit (`change`). Scaling
  is refused on workloads Argo CD, Flux or Helm manage, or an autoscaler targets.
- Approvals (`effect: approve`): the call becomes an approval request answered with its
  id and diff; an approver named by the binding decides with `decide_approval`
  (`list_approvals` shows requests); the requester calls again with `approval: <id>`,
  which runs once, only for the same arguments and an unchanged target, before
  `approvals.ttl` (1h by default). Approvers can withdraw an approval before it is used;
  a requester has at most 20 pending requests. Remote mode only.
- `resolve_source` and `propose_change`, with forges configured in `providers.forges`
  (GitHub, Gitea or Forgejo, GitLab; token by reference, explicit repository list).
  `resolve_source` finds the files that declare a workload, through its Argo CD
  Application (`providers.delivery`), an `opsward/source` annotation or
  `sourceResolution.static`, lists them and returns the ones asked for.
  `propose_change` applies exact text replacements to those files and returns the diff
  (`change.strategy: suggest`, risk read) or opens a pull request naming the caller and
  the audit record (`pull-request`, risk write); nothing is applied to the cluster.
  Values of `kind: Secret` manifests are blanked in what is shown. An Argo CD tracking id
  must name the workload and its Application deploy to the workload's namespace; the
  `app.kubernetes.io/instance` label is used with `providers.delivery.labelTracking`.
- The audit `change` records the URL of a proposed change.

## [0.3.0] - 2026-10-16

What an agent needs to find its way: discovery tools that show each caller only what it may
work on, results sized for an agent to read, and queries across a cluster where policy
allows them.

### Added

- `list_environments`, `list_namespaces` and `list_workloads`, authorized per item: each
  shows only what the caller may work on. A listing runs once per profile the caller's
  bindings use and keeps an item only when allowed through the profile it was read with;
  the audit records how many items were read and kept. `list_workloads` without a
  namespace spans the environment, and `unhealthy` keeps only workloads with fewer ready
  replicas than desired. The reader needs `list` on namespaces, and on Deployments,
  StatefulSets and DaemonSets cluster-wide for environments that span every namespace.
- Metrics and logs backends per cluster (`providers.kubernetes.clusters.<name>.metrics`
  and `.logs`), used instead of the shared `providers.metrics` and `providers.logs`; a
  cluster label is needed only where one backend serves several clusters.
- `query_metrics` and `query_logs` without a namespace query the whole cluster, where the
  environment spans every namespace and an allow covering every namespace applies; any
  deny or approval in the environment, whatever it is scoped to, still applies.

### Changed

- Metrics results are summaries by default: each series has `min`, `max`, `avg`, `last`
  and `samples`; points are returned with `points: true`.
- Log lines longer than 2,000 bytes are cut; `query_logs` results past 32 KiB
  (JSON-encoded) leave out the oldest lines, counted in `linesOmitted`; container logs are
  capped at 16 KiB per container and 32 KiB per call (were 256 KiB and 1 MiB).

## [0.2.0] - 2026-10-16

A gateway a team can share: MCP over HTTP with API tokens or OIDC, internal groups, and a
PostgreSQL store for more than one replica.

### Added

- Remote mode (`identity.mode: remote`): MCP over Streamable HTTP at `/mcp`, stateless, with
  `/healthz` and `/readyz`, optional TLS whose certificate is re-read when it changes.
- API tokens: `opsward token create|list|revoke`; only a hash of each secret is stored, every
  token expires, and creating or revoking one is audited.
- OIDC access tokens (`identity.oidc`): JWTs from any compliant issuer, bound to the MCP
  resource URL as audience, only access-token types accepted, claim groups prefixed
  `oidc:` by default; RFC 9728 protected resource metadata so that MCP clients discover the
  issuer.
- Internal groups (`identity.groups`), applied on reload.
- PostgreSQL store (`storage.type: postgres`, DSN by file or environment variable) for more
  than one replica: migrations under an advisory lock, one audit chain shared by replicas.
- Audit records name the credential behind a call (`principal.credential`).
- Subjects that are email addresses are compared without case in policy and groups.
- `opw_` API tokens are redacted from tool output and caught by the secret scanner.

### Fixed

- `diagnose_workload` reads crash logs only for pods failing now or whose container ended
  within the hour, and logs of a previous instance are marked `previous`.
- `query_logs` asks Loki for categorized labels, so per-line metadata no longer splits
  every line into its own stream.
- Tool output drops terminal escape sequences and is compact JSON.
- Out-of-scope or malformed queries are audited as `invalid`, not `error`.

## [0.1.0] - 2026-10-15

First release: a single-user, read-only gateway over MCP stdio.

### Added

- Repository scaffolding: Go module, lint and format configuration, secret scanning hooks,
  CI, release and container build configuration.
- Policy engine: roles and bindings scoped to environments, namespaces, workloads and
  applications, with optional CEL conditions; default deny.
- Hash-chained audit to an embedded SQLite store and a stream, with retention checkpoints
  and `opsward audit verify`.
- `get_workload_status` over MCP stdio, with output redaction.
- `opsward validate` for configuration and policy.
- `list_workload_events` (workload, its ReplicaSets and pods) and `list_workload_logs`
  (recent lines per container, including the previous instance).
- Knowledge packs: a builtin generic pack and operator paths, searchable with
  `search_knowledge` and `get_knowledge`, and attached to results as notes when their CEL
  condition matches.
- Configuration reload on file change or `SIGHUP`, all-or-nothing; identity, storage and
  audit changes require a restart.
- Prometheus-compatible metrics and Loki logs backends: `get_workload_metrics`,
  `query_metrics` and `query_logs`, with PromQL and LogQL confined to the caller's namespace
  and cluster before they are sent.
- `diagnose_workload`: status, events, failing containers' logs and resource use in one call.

[Unreleased]: https://github.com/quyendv/opsward/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/quyendv/opsward/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/quyendv/opsward/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/quyendv/opsward/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/quyendv/opsward/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/quyendv/opsward/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/quyendv/opsward/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/quyendv/opsward/releases/tag/v0.1.0
