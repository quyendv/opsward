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
    audience: https://opsward.example.com/mcp   # default: publicURL + /mcp
    subjectClaim: email
    groupsClaim: groups
    groupsPrefix: "oidc:"
    scopes: [opsward]          # optional; advertised to clients
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
  must be written within 2 minutes. Each call has a deadline ten seconds shorter, so a
  slow call ends as an audited error the client can still read.
- A request carries one JSON-RPC message. Batches, which only the oldest protocol version
  allows and no client sends, are rejected.
- TLS is optional, because most deployments terminate it at an ingress. Plain HTTP on a
  non-loopback address is accepted with a warning. Certificate files are re-read when they
  change, so a rotated certificate needs no restart.
- On `SIGTERM` the listener stops accepting and in-flight calls get 30 seconds to finish.
  Calls still running then are cancelled and get five seconds to write their outcome
  records before connections are closed.

## Authentication

Every `/mcp` request needs `Authorization: Bearer <token>`. A token that starts with `opw_` is
an API token; any other token is treated as an OIDC access token when OIDC is configured and
rejected otherwise. A missing or rejected token gets `401` with a `WWW-Authenticate` header,
which points to the resource metadata when OIDC is configured. When the store cannot be
read to decide, the answer is `503`: the token may be valid.

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
revocations and audit records. `secret` is 52 random base32 characters, 256 bits. The store keeps the SHA-256 of
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

`create` prints the token once, alone on stdout. `revoke` takes the ID or the whole token;
given the token, it revokes by the ID inside it and never records the secret. Audit records
of these commands go to stderr when the configured stream is stdout, so that they cannot
mix with the token; to have them reach the same collector as the server's records, use a
`file` stream on a path both share. The command works on the store directly, so it is
available to whoever can open the store, the same people who could already read or rewrite
it. Creating and revoking a token is written to the audit chain (`create_api_token`,
`revoke_api_token`, risk `destructive`), with `adapter: cli` and the operating system user
as subject (`os:<user>`), so that a token found in a record can be traced to its creation.
Inside a container that user is usually the image's, such as `nonroot`; the person is then
identified by the platform's own record of the session, such as the Kubernetes audit log of
the `exec`.

A client keeps the token it sends as a header in its own configuration, usually in plain
text. Give each person and pipeline its own token with a short lifetime, so that one copy
can be revoked without touching the others.

### OIDC

Opsward is an OAuth resource server (MCP authorization, RFC 9728). It validates access
tokens issued by `identity.oidc.issuer`, which must be JWTs; an issuer that hands out
opaque access tokens needs a broker in front that issues JWTs:

- signature against the issuer's published keys, found by discovery. The keys are fetched
  again only for a key ID not held yet, and at most once every 30 seconds: a forged token
  must not make Opsward call the issuer on every request;
- a JWT `typ` of none, `JWT` or `at+jwt`, and no `typ` claim other than `Bearer` or
  `at+jwt`, so that ID tokens are refused whatever their audience;
- an algorithm the issuer lists in `id_token_signing_alg_values_supported`, never a
  symmetric one or `none`;
- `iss` equal to the issuer, `aud` containing `audience`, `exp` in the future (`nbf` is
  allowed five minutes of clock skew);
- when `subjectClaim` is `email`, `email_verified` must be `true`.

`audience` defaults to the MCP resource URL, `publicURL` + `/mcp`, which MCP clients send
as the resource indicator (RFC 8707) when they request a token. Binding tokens to that
audience keeps tokens issued for other applications of the same issuer, ID tokens
included, from being accepted. Do not set `audience` to a client ID: tokens that client obtains for
itself would then be accepted too.

The issuer is discovered at start-up. If it cannot be reached, Opsward starts anyway and
retries in the background, waiting from one second up to a minute between attempts; until
discovery succeeds OIDC tokens are rejected and API tokens work.

Clients find the issuer through `/.well-known/oauth-protected-resource/mcp` (also served
without the `/mcp` suffix), which names `publicURL` + `/mcp` as the resource, the issuer as
its only authorization server, and `scopes` when set. Every `401` carries
`WWW-Authenticate: Bearer resource_metadata="…"` pointing there. Without OIDC neither the
metadata nor the parameter is served.
Registering the client is between the client and the issuer: a pre-registered client ID,
Client ID Metadata Documents or Dynamic Client Registration, whichever both support. Opsward
does not register clients, issue tokens or proxy an authorization server (0002). A client
that can do none of these can send an API token as a static header.

### Groups

A principal's groups are the union of:

1. the token's groups: those given at `opsward token create`, or the `groupsClaim` values of
   an OIDC token, each prefixed with `groupsPrefix`, `oidc:` unless set. At many issuers
   people can create groups themselves; without a prefix, a group named `platform` there
   would be the internal group `platform`. Set `groupsPrefix: ""` only for an issuer whose
   groups only administrators manage;
2. the internal groups whose member list contains the subject, from `identity.groups`.

A binding without `subjects` applies to every authenticated principal. With OIDC that is
everyone the issuer will issue a token for this audience, which at a multi-tenant issuer or
one that registers clients dynamically can be more people than intended; name subjects or
groups in such bindings.

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

The DSN may contain a password, so it is referenced, never inline. Use
`sslmode=verify-full` for a database across a network: the store holds the API token
hashes that authentication trusts.

Migrations are embedded and applied at start-up in one transaction under an advisory lock,
so replicas starting together apply each one once. Appending to an audit chain takes a
transaction-scoped advisory lock on the chain's writer ID, in a READ COMMITTED transaction
whatever the database's default, which gives replicas sharing a chain the same ordering the
SQLite store gets from its single writer.

Sessions get `idle_in_transaction_session_timeout` and TCP keepalive settings unless the
DSN sets its own, so that a replica that dies inside a transaction releases its locks in
seconds rather than when the operating system's keepalive notices, about two hours later.

Times are stored as integer nanoseconds in both stores, the precision of the records
themselves; Postgres timestamps keep only microseconds. The DSN is read at start-up: a
rotated database password applies after a restart. Both stores pass one conformance test
suite. CI runs it against a Postgres service and fails if the database is missing; locally
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
