# Opsward

**A guarded gateway that lets AI agents operate your infrastructure, within the permissions you grant.**

> Status: pre-alpha. Read-only workload, metrics and logs tools and knowledge packs work over MCP stdio, or over HTTP for a team with API tokens; nothing is stable.

## Why

AI agents are already good at reading logs, querying metrics and editing deployment
manifests. What stops a team from sharing that with everyone is not capability but trust:
most tooling hands the agent one engineer's credentials and a free-form command line, so the
agent can do whatever that engineer can, nobody else can safely use it, and nothing records
what happened.

Opsward sits between agents and infrastructure and makes that trust explicit:

- **Typed operational tools** instead of a raw command channel, each with a declared risk class.
- **Per-principal policy** from your identity provider, on top of each backend's own RBAC.
- **Audit** of every call, allowed or denied, attributed to a person.
- **Redaction** so secret values never reach a model.
- **Propose, don't mutate**: changes become pull requests against your GitOps source of truth.
- **Operational knowledge packs**: your runbooks and hard-won lessons, available to every agent.

It speaks MCP, so it works with the agent clients your team already uses. The engine itself
is protocol-agnostic.

## Try it

Download a binary from [Releases](https://github.com/quyendv/opsward/releases), use the
container image `ghcr.io/quyendv/opsward`, or build from source with `mise run build`.

```sh
opsward validate --config examples/opsward.yaml
```

Point `providers.kubernetes.clusters.*.profiles.reader.kubeconfig` at a kubeconfig whose
credential is bound to a read-only role, then register `opsward serve --config <file>` as a
stdio MCP server in your agent client. Every call is audited to the SQLite file in
`storage.path` and to stderr; `opsward audit verify --config <file>` checks the chain.

To share one Opsward with a team, run it in remote mode
([examples/opsward-remote.yaml](examples/opsward-remote.yaml)): it serves MCP over HTTP at
`/mcp`, and each person or pipeline authenticates with an API token from
`opsward token create`, sent as `Authorization: Bearer <token>`. For example, in Claude Code:

```sh
claude mcp add --transport http opsward https://opsward.example.com/mcp \
  --header "Authorization: Bearer $OPSWARD_TOKEN"
```

The client stores that header in its configuration; give each person their own token.

Tools: `get_workload_status`, `list_workload_events`, `list_workload_logs`,
`diagnose_workload`, `search_knowledge`, `get_knowledge`; with a Prometheus-compatible
metrics backend also `get_workload_metrics` and `query_metrics`, and with Loki `query_logs`.
Free-form queries are rewritten so that they cannot leave the caller's namespace. Results that match a knowledge document carry its
summary as a note. Edits to the configuration file apply without a restart; after editing
knowledge documents, send `SIGHUP`.

## Development

Requires [mise](https://mise.jdx.dev).

```sh
mise run setup   # tools and git hooks
mise run check   # everything CI runs
mise run build
```

See [AGENTS.md](AGENTS.md) for architecture invariants and conventions, and
[CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## License

[Apache License 2.0](LICENSE)
