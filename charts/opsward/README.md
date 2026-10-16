# opsward

Runs [Opsward](https://github.com/quyendv/opsward) in remote mode: MCP over HTTP at `/mcp`,
for a team with API tokens or OIDC. Design note:
[0011](https://github.com/quyendv/opsward/blob/main/docs/design/0011-running-in-a-cluster.md).

```sh
helm install opsward oci://ghcr.io/quyendv/charts/opsward -n opsward --create-namespace -f values.yaml
```

## Values that matter

| Value | What it does |
|---|---|
| `config` | The Opsward configuration. `identity.mode` must be `remote`; `server.http.address` defaults to `0.0.0.0:8080`, `storage.path` to `/var/lib/opsward/opsward.db`. Policy, environments, groups and source resolution reload in place; other sections roll the pod. |
| `localProfiles` | Profiles in this cluster. Each gets a ServiceAccount `opsward-<profile>`, a token and a kubeconfig at `/etc/opsward/kubeconfig/<profile>/kubeconfig` for `config` to name. |
| `rbac.rendered` | The reviewed output of `opsward rbac render --cluster <this cluster> --service-account-namespace <namespace>`, installed with the release (`--set-file rbac.rendered=rbac.yaml`). Only RBAC objects binding the profiles' ServiceAccounts are accepted; the chart never derives RBAC itself. |
| `extraSecretMounts` | Secrets mounted read-only for credential files `config` references: other clusters' kubeconfigs, forge tokens, a Postgres DSN. |
| `persistence` | The volume for SQLite. With `storage.type: postgres` it can be disabled and `replicaCount` raised. |
| `ingress` | An optional Ingress to the Service. |

The pod runs as non-root with a read-only root filesystem, no capabilities and no token of
its own: Opsward acts only through its profiles.

Installing from a source checkout needs `--set image.tag=<a released version>`: the
chart's own version there is a development one with no image.

Create an API token:

```sh
kubectl -n opsward exec deploy/opsward -- /opsward token create \
  --config /etc/opsward/config/opsward.yaml --subject alice@example.com
```
