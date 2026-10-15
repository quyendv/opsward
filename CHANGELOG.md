# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Remote mode (`identity.mode: remote`): MCP over Streamable HTTP at `/mcp`, stateless, with
  `/healthz` and `/readyz`, optional TLS whose certificate is re-read when it changes.
- API tokens: `opsward token create|list|revoke`; only a hash of each secret is stored, every
  token expires, and creating or revoking one is audited.
- Internal groups (`identity.groups`), applied on reload.
- Audit records name the credential behind a call (`principal.credential`).

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

[Unreleased]: https://github.com/quyendv/opsward/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/quyendv/opsward/releases/tag/v0.1.0
