# Opsward

**A guarded gateway that lets AI agents operate your infrastructure, within the permissions you grant.**

> Status: pre-alpha, design phase. Not usable yet.

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
