# 0005: Remote access

- Status: accepted
- Date: 2026-10-15
- Extends: [0001](0001-architecture.md), [0002](0002-identity-policy-storage.md)

## Summary

v0.2 lets a team share one Opsward: MCP over HTTP, several principals authenticated by API
token or OIDC, internal groups, and a Postgres store for more than one replica. Nothing in
the engine, policy or tools changes; the work is at the edges.

| Milestone | Scope |
|---|---|
| M4 | `identity.mode: remote`, MCP over Streamable HTTP, API tokens and `opsward token`, internal groups |
| M5 | OIDC access tokens, protected resource metadata |
| M6 | Postgres store |

## Modes

`identity.mode` decides how `opsward serve` listens:

| Mode | Transport | Principal |
|---|---|---|
| `local` (default) | MCP over stdio | fixed in `identity.principal` |
| `remote` | MCP over HTTP | from the bearer token of each request |

There is no unauthenticated HTTP mode and no stdio mode with tokens. A fixed principal on a
network listener would give everyone who can reach the port that principal's permissions.

```yaml
identity:
  mode: remote
  apiTokens:
    maxTTL: 365d               # default; a token can never be created for longer
  oidc:                        # optional, M5
    issuer: https://id.example.com
    audience: opsward
    subjectClaim: email
    groupsClaim: groups
    groupsPrefix: "oidc:"
  groups:                      # internal groups, reloadable
    platform: [alice@example.com, bob@example.com]

server:
  http:
    address: 0.0.0.0:8080
    publicURL: https://opsward.example.com   # required with oidc
    tls:                                     # optional; usually terminated in front
      certFile: /etc/opsward/tls/tls.crt
      keyFile: /etc/opsward/tls/tls.key
```

## Serving MCP over HTTP

The MCP endpoint is `/mcp`, served by the SDK's Streamable HTTP handler in **stateless**
mode: each request carries its own token and is handled on its own, with no session to
keep. Opsward never sends requests to the client, which is the one thing sessions are
needed for, and without them replicas need no sticky routing.

| Path | Authentication | Purpose |
|---|---|---|
| `/mcp` | bearer token | MCP |
| `/.well-known/oauth-protected-resource`, and the same with `/mcp` appended | none | RFC 9728 metadata, with OIDC only |
| `/healthz` | none | the process is serving |
| `/readyz` | none | the store answers |

- Cross-origin browser requests are rejected (Go's `http.CrossOriginProtection`), and the
  SDK's DNS rebinding protection stays on.
- Requests are limited to 4 MiB, headers must arrive within 10 seconds, and a response
  must be written within 2 minutes.
- TLS is optional, because most deployments terminate it at an ingress. Plain HTTP on a
  non-loopback address is accepted with a warning. Certificate files are re-read when they
  change, so a rotated certificate needs no restart.
- On `SIGTERM` the listener stops accepting and in-flight calls get 30 seconds to finish.

## Authentication

Every `/mcp` request needs `Authorization: Bearer <token>`. A token that starts with `opw_` is
an API token; any other token is treated as an OIDC access token when OIDC is configured and
rejected otherwise. A missing or rejected token gets `401` with a `WWW-Authenticate` header,
which points to the resource metadata when OIDC is configured.

The reason a token was rejected is logged with the client address, never echoed to the
client, and never logged with the token itself. A rejected request is not an audit record:
audit records attribute calls to principals, and an unauthenticated request has none.

The principal records how it authenticated, so a call can be traced to the token behind it:

```json
"principal": { "subject": "alice@example.com", "groups": ["platform"], "adapter": "mcp-http", "credential": "token:abcdabcdabcd" }
```

`credential` is `token:<id>` or `oidc`, and absent in local mode.

### API tokens

```
opw_<id>_<secret>
```

`id` is 12 random base32 characters and is public: it names the token in listings,
revocations and audit records. `secret` is 32 random bytes. The store keeps the SHA-256 of
the secret, never the secret: a secret with 256 bits of entropy cannot be guessed from its
hash, so a slow password hash would add cost without adding safety. Comparison is constant
time. The `opw_` prefix lets secret scanners recognize a leaked token.

Each token has a subject, groups, a description, a creation time and an expiry. Expiry is
mandatory: 90 days by default, at most `identity.apiTokens.maxTTL`. Revocation takes effect
on the next request, because every request reads the store and nothing is cached. The last
use is recorded, at most once a minute per token.

```sh
opsward token create --config FILE --subject ci@example.com --group deployers --ttl 30d --description "release pipeline"
opsward token list   --config FILE
opsward token revoke --config FILE <id>
```

`create` prints the token once. The command works on the store directly, so it is
available to whoever can open the store, the same people who could already read or rewrite
it. Creating and revoking a token is written to the audit chain (`create_api_token`,
`revoke_api_token`, risk `destructive`), with `adapter: cli` and the operating system user
as subject, so that a token found in a record can be traced to its creation.

### OIDC

Opsward is an OAuth resource server (MCP authorization, RFC 9728). It validates access
tokens issued by `identity.oidc.issuer`:

- signature against the issuer's published keys, found by discovery and refreshed when an
  unknown key ID appears;
- `iss` equal to the issuer, `aud` containing `audience`, `exp` in the future, with one
  minute of clock skew;
- when `subjectClaim` is `email`, `email_verified` must be `true`.

The issuer is discovered at start-up. If it cannot be reached, Opsward starts anyway and
retries in the background; until discovery succeeds OIDC tokens are rejected and API tokens
work.

Clients find the issuer through `/.well-known/oauth-protected-resource`, which names
`publicURL` + `/mcp` as the resource and the issuer as its only authorization server.
Registering the client is between the client and the issuer: a pre-registered client ID,
Client ID Metadata Documents or Dynamic Client Registration, whichever both support. Opsward
does not register clients, issue tokens or proxy an authorization server (0002). A client
that can do none of these can send an API token as a static header.

### Groups

A principal's groups are the union of:

1. the token's groups: those given at `opsward token create`, or the `groupsClaim` values of
   an OIDC token, each prefixed with `groupsPrefix`;
2. the internal groups whose member list contains the subject, from `identity.groups`.

Subjects and group names are compared exactly. `identity.groups` is the one part of
`identity` that a reload applies, because membership changes far more often than how
people sign in.

## Postgres

`storage.type: postgres` stores everything the SQLite store does, for more than one
replica.

```yaml
storage:
  type: postgres
  dsn: { file: /etc/opsward/postgres-dsn }   # or { env: OPSWARD_POSTGRES_DSN }
```

The DSN may contain a password, so it is referenced, never inline. Migrations are embedded
and applied at start-up under an advisory lock, so replicas starting together apply each
one once. Appending to an audit chain takes a transaction-scoped advisory lock on the
chain's writer ID, which gives replicas sharing a chain the same ordering the SQLite store
gets from its single writer.

Both stores pass one conformance test suite. CI runs it against a Postgres service; locally
it runs when `OPSWARD_TEST_POSTGRES_DSN` is set.

## Consequences

- One deployment serves a team, each call attributed to a person or a named token.
- Revoking a person's access means removing them at the issuer, from internal groups, and
  revoking their API tokens; `opsward token list` shows what a subject holds.
- With OIDC, clients that support neither pre-registered clients, metadata documents nor
  dynamic registration at the team's issuer need API tokens.
- A replica set needs Postgres; SQLite stays one replica.

## Alternatives considered

- **Sessions (stateful Streamable HTTP).** Needed only for server-initiated requests, which
  Opsward does not make, and they would tie a client to a replica.
- **Opsward as an authorization server.** Would let every MCP client register dynamically,
  at the cost of issuing tokens, storing client registrations and becoming part of the
  identity provider. Rejected in 0002 and still rejected.
- **API tokens hashed with a password hash.** Appropriate for low-entropy secrets chosen by
  people; for random 256-bit secrets it only adds latency to every request.
- **Signed (stateless) API tokens.** No store read per request, but revocation would need a
  deny list anyway, and a store read per request is cheap next to a Kubernetes call.
