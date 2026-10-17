# Opsward

**A guarded gateway that lets AI agents operate your infrastructure, within the permissions you grant.**

> Status: pre-alpha. Workload, metrics and logs tools, knowledge packs, narrow changes with approvals, pull requests against a GitOps source and alert investigation work over MCP stdio, or over HTTP for a team with a web console for approvals and audit; nothing is stable.

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
credential is bound to a read-only role (`opsward rbac render --config <file>` prints the
RBAC each profile needs for what your policy allows), then register `opsward serve --config <file>` as a
stdio MCP server in your agent client. Every call is audited to the SQLite file in
`storage.path` and to stderr; `opsward audit verify --config <file>` checks the chain.

To share one Opsward with a team, run it in remote mode
([examples/opsward-remote.yaml](examples/opsward-remote.yaml)): it serves MCP over HTTP at
`/mcp`, and each person or pipeline authenticates with an API token from
`opsward token create`, sent as `Authorization: Bearer <token>`, or signs in through an
OpenID Connect issuer configured in `identity.oidc`. With an API token in Claude Code:

```sh
claude mcp add --transport http opsward https://opsward.example.com/mcp \
  --header "Authorization: Bearer $OPSWARD_TOKEN"
```

The client stores that header in its configuration; give each person their own token.
A profile with `impersonate` acts as the caller in its cluster, so the cluster's RBAC
decides per person and its audit log names them (design note
[0014](docs/design/0014-impersonation.md)).
With a `console` section, approvers decide requests and auditors search the audit in a
browser at `/console/`, signed in through the identity provider or with an API token. The
console calls the same tools as the agents, under the same policy and audit
(`search_audit` is granted only to roles that name it; design note
[0015](docs/design/0015-web-console.md)).

To run it in a cluster, install the Helm chart (`charts/opsward`, design note
[0011](docs/design/0011-running-in-a-cluster.md)):

```sh
helm install opsward oci://ghcr.io/quyendv/charts/opsward -n opsward --create-namespace -f values.yaml
```

`values.yaml` holds the configuration under `config` and names the profiles to create in
that cluster under `localProfiles`; `opsward rbac render --cluster <it>
--service-account-namespace opsward` prints the RBAC they need.

Tools: `list_environments`, `list_namespaces`, `list_workloads` (each showing only what the
caller may work on), `get_workload_status`, `list_workload_events`, `list_workload_logs`,
`diagnose_workload`, `search_knowledge`, `get_knowledge`, `restart_workload` and
`scale_workload` (previewed, and approved by someone else where policy says so; approvers
use `list_approvals` and `decide_approval`, or the web console), `search_audit`; with a forge configured, `resolve_source`
and `propose_change`, which turn a change into a diff or a pull request against the
workload's GitOps source; with Argo CD configured, `get_delivery_status`; with a
Prometheus-compatible
metrics backend also `get_workload_metrics` and `query_metrics`, and with Loki `query_logs`.
In remote mode Opsward can also take Alertmanager's alerts (`alerts` in the
configuration, design note [0012](docs/design/0012-alert-investigation.md)): each firing
alert is mapped to its workload and diagnosed, and the result goes to a webhook or Slack.
With agent mode on (`agent`, design note [0013](docs/design/0013-agent-mode.md)), a model
then looks further with the read tools and adds a short analysis.
Free-form queries are rewritten so that they cannot leave the caller's namespace and
cluster; without a namespace they span the cluster, where the environment and policy allow. Results that match a knowledge document carry its
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
