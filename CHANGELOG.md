# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `list_environments`, `list_namespaces` and `list_workloads`, authorized per item: each
  shows only what the caller may work on. The reader profile needs `list` on namespaces.
- `query_metrics` and `query_logs` without a namespace query the whole cluster, where the
  environment spans every namespace and a binding not restricted to namespaces allows it.

### Changed

- Metrics results are summaries by default: each series has `min`, `max`, `avg`, `last`
  and `samples`; points are returned with `points: true`.

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

[Unreleased]: https://github.com/quyendv/opsward/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/quyendv/opsward/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/quyendv/opsward/releases/tag/v0.1.0
