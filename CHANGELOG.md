# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Repository scaffolding: Go module, lint and format configuration, secret scanning hooks,
  CI, release and container build configuration.
- Policy engine: roles and bindings scoped to environments, namespaces, workloads and
  applications, with optional CEL conditions; default deny.
- Hash-chained audit to an embedded SQLite store and a stream, with retention checkpoints
  and `opsward audit verify`.
- `get_workload_status` over MCP stdio, with output redaction.
- `opsward validate` for configuration and policy.
