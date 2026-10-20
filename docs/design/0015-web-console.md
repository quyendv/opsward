# 0015: Web console

- Status: accepted
- Date: 2026-10-17
- Extends: [0001](0001-architecture.md) (adapters), [0005](0005-remote-access.md), [0007](0007-operate-and-approvals.md)

## Summary

A browser console for the people around the agents: approvers decide requests with the
diff in front of them, and auditors search what was done. It is an adapter like MCP. Every
page that reads Opsward's state or decides calls a tool through the engine as the
signed-in principal (`principal.adapter == "console"`), so policy, approvals and audit apply
exactly as they do for an agent. The console holds no data and no authority of its own.

```yaml
server:
  http:
    address: ":8080"
    publicURL: https://opsward.example.com
console:
  sessionTTL: 8h                 # the default; at most 24h
  oidc:                          # sign in with identity.oidc's issuer
    clientID: opsward-console
    clientSecret: { file: /etc/opsward/console/client-secret }   # omit for a public client
    scopes: [email, groups]      # requested besides openid
  apiTokens: false               # true: also sign in with an API token
```

The console is served under `/console/` by the remote listener, and only in remote mode.
It is restart-only, like the rest of `server`.

## Pages

- **Approvals**: the requests the person made or may decide (`list_approvals`), each with
  its diff, and a form to approve, deny or withdraw with a reason (`decide_approval`). A
  requester sees the state of their requests; the agent still makes the approved call
  (0007).
- **Audit**: records searched by time, principal, tool, status, environment and namespace
  (`search_audit`, below), newest first, and one record with its decision, outcome and
  change.
- **You**: the signed-in principal, its groups, how it signed in and when the session ends,
  and the bindings that name it. What policy says about a person is shown to that person,
  read from the policy in force rather than through a tool; other people's bindings are
  not.

Policy is not edited in the console. The configuration file is its source of truth and is
changed through review like any other declarative source; a console that wrote policy
would be a second path to escalate (0001, threat model).

## Searching the audit

`search_audit` is a read tool, available over MCP too, so an agent can answer "who
restarted this yesterday". Its arguments are filters: `since` and `until` (the last day by
default), `subject`, `tool`, `status`, `environment`, `namespace`, and a `before` cursor.
It returns the window it searched and at most 100 records, newest first, fewer when they
would pass 32 KiB JSON-encoded (the bound of 0006: an agent cannot read a larger result,
and a record of a change runs to kilobytes). It reads at most 5,000 records from the store
per call; when it stops early it says so and returns the cursor to continue, which keeps
the search's time window. Agent mode does not offer it: records would reach the model
provider. Checkpoint
records are left out: `opsward audit verify` is for the chain.

Records describe other people's work, which is not the same as reading a workload, so:

- **A role grants `search_audit` only by naming it.** `risk: [read]` does not include it;
  a viewer does not become an auditor. Naming is needed to grant, not to take away: a
  binding that denies `risk: [read]` covers it too.
- **A caller no binding grants it to is denied** before anything is read.
- **Each record is decided as a call at its own scope.** It is kept only when policy allows
  `search_audit` there: a record in a namespace needs a grant covering the namespace, one
  for a whole environment a grant covering every namespace of it, and one with no
  environment (approval tools, knowledge search, unknown tools) a grant limited to no
  environment. A team's auditors see their namespaces and nothing wider. Records do not
  keep the target's labels, so a grant narrowed to `apps` shows none. The limit of 100
  counts the records kept, and a search offers to continue only when one more kept record
  exists or it stopped reading.
- Records are already redacted when written (0002). Searching the audit is itself audited.

## Signing in

**OIDC.** The console is a client of `identity.oidc`'s issuer: authorization code flow
with PKCE, a `state` and a `nonce`, with redirect URL `<publicURL>/console/callback`. It
verifies the ID token, whose audience is `clientID`, and maps it to a principal with the
same `subjectClaim`, `groupsClaim`, `groupsPrefix` and verified-email rule as access
tokens, so a person is the same principal in the console as through their agent. Approvals
compare subjects (0007): the subject claim must be the same in both tokens. `sub` is not,
for issuers that issue pairwise subjects per client; use a claim that is, such as `email`
or an object ID. The subject and groups come from the ID token, so `scopes` must ask for
what puts them there (`email` and `groups` with many issuers).

**API tokens**, when `apiTokens` is set: a form takes the token, which is checked as for
MCP. The session remembers the token's ID and checks on every request that the token is
neither revoked nor expired.

**Sessions** are kept in the store, so they work across replicas and end when the person
signs out. The cookie `__Host-opsward-session` (`Secure`, `HttpOnly`, `SameSite=Lax`)
carries a random identifier; the store keeps its hash. A session lasts `sessionTTL`, and
never past the API token's expiry. Groups from the issuer are fixed for the session, so a
person removed from a group there keeps it until the session ends; internal groups are
looked up on every request, and policy is the current one, so editing either takes effect
at once. A session ends when the way it signed in is no longer enabled. Sign-ins are
logged; the calls a person makes are audited as any other.

## Browser protections

- Pages are rendered on the server from embedded templates. There is no JavaScript, and
  the content security policy allows none: `default-src 'none'; style-src 'self';
  img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`.
- Every request that changes something is a `POST` checked by Go's cross-origin
  protection, which rejects requests a browser marks as sent from another site.
- Pages are `Cache-Control: no-store` and `Referrer-Policy: no-referrer`.
- Approval requests and records are shown as data: templates escape them, and a diff is
  preformatted text.

## Alternatives considered

- **A single-page application** (React or similar) over a JSON API. It needs a Node build
  in the toolchain and in CI, and third-party code in the page that can read whatever the
  page can; the pages here are tables and forms, which the server renders without either.
- **Tokens kept in the browser** and sent as bearer tokens. Script on the page could read
  them, and an access token taken from the console works for MCP too. A session cookie the
  page cannot read is worth the store table.
- **A sealed cookie instead of stored sessions.** No table, but signing out cannot end a
  session, and replicas must share a key.
- **A REST API beside the tools.** It would be a second authorization path to keep in step
  with the engine; calling the tools keeps one (invariant 4).
