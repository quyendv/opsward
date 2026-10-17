# 0014: Acting as the caller in Kubernetes

- Status: accepted
- Date: 2026-10-17
- Extends: [0002](0002-identity-policy-storage.md) (credential sources), [0009](0009-rbac-render.md)

## Summary

With profiles, a cluster sees Opsward's credential: its RBAC decides per profile, and its
audit log names the profile's ServiceAccount, not the person who asked (0002). A profile can
instead impersonate the caller. Every request it makes then carries `Impersonate-User` and
`Impersonate-Group`, so the cluster authorizes the person and its audit log names them, with
the profile's credential as the impersonator. Opsward's policy still decides first; the
cluster's RBAC decides second, per person rather than per profile.

```yaml
providers:
  kubernetes:
    clusters:
      cluster-a:
        profiles:
          reader:
            kubeconfig: /etc/opsward/kubeconfig/reader/kubeconfig
            impersonate:
              usernamePrefix: "opsward:"   # the default
              groupPrefix: "opsward:"      # the default
```

## Who is impersonated

- **The user** is `usernamePrefix` followed by the principal's subject, normalized as policy
  matches it (an address in lower case). The default prefix keeps Opsward's principals apart
  from the cluster's users: a principal called `admin` does not become the cluster's
  `admin`. With the username prefix the API server's OIDC configuration uses (`oidc:`, say),
  a person reaches the RBAC bound to their own user name, provided Opsward's subject claim
  is the API server's username claim, and the name is not an address whose case matters.
- **The groups** are the principal's groups that a binding granting tools through this
  profile names, each after `groupPrefix`. The reader also serves every call's label lookup
  when policy reads labels, and the delivery profile reads Applications for every caller of
  `get_delivery_status`, `resolve_source` and `propose_change`, so they send the groups of
  every principal policy grants tools to: in the reader's cluster, or anywhere for the
  delivery profile. Other groups are not sent: the credential may
  impersonate only the groups listed for it, and one group it may not impersonate fails the
  whole request. The API server adds `system:authenticated` itself. Groups are Opsward's:
  internal groups, a token's groups, and the identity provider's groups after
  `identity.oidc.groupsPrefix`; `groupPrefix` comes on top of that one. The cluster takes
  them as Opsward asserts them, as from any proxy it lets impersonate, so whoever edits
  Opsward's groups changes what people may do in the cluster. Keeping the default
  `opsward:` keeps those groups apart from the cluster's own.
- A name that would start with `system:`, the cluster's own identities, is refused, and so is
  a request that carries no principal: an impersonating profile never falls back to acting as
  itself. Calls nobody waits on (alert investigations, agent mode) act as their principal,
  the alerts token's subject.
- A kubeconfig that sets impersonation itself (`as`, `as-groups`) is refused for such a
  profile: two sources would decide who a request acts as.

The reader's label lookup before authorization (0006) runs as the caller too, so a person
the cluster does not let read a workload is decided by policy as for unknown labels. A
listing across an environment keeps the kinds and namespaces the cluster lets the caller
list, and is refused only when it lets them list none; a profile acting as itself still
fails when refused, since that is a misconfiguration. An environment with namespace
patterns lists the cluster's namespaces first, so its callers need `list` on namespaces.

Approvals do not lend the approver's permissions: the dry run, the approved retry and the
change all act as the requester, so the cluster must let the requester make the change.
An approval is a second gate, not a grant.

## What the credential needs

The profile's own credential needs only `impersonate` on `users` and `groups`, with
`resourceNames` naming the prefixed users and groups that bindings granting through the
profile name. A binding granted to groups, or to every principal, names no users, so the
users rule then has no `resourceNames`: the credential can act as any user name, the
cluster's own `system:` users included. Opsward never sends one, but whoever holds the
credential could, so it deserves the care of an administrator's. Groups stay limited to
those listed, and `rbac render` refuses to grant a `system:` user or group. A binding that
denies a role in a whole environment does not narrow these lists: they name a few more
people than can reach the profile, never fewer. The people need
their own RBAC for what the tools do.

`opsward rbac render` renders, for an impersonating profile, the ClusterRole
`opsward:<profile>:impersonate` and its ClusterRoleBinding to the profile's subject, and the
rules the profile's tools need as the ClusterRole `opsward:<profile>`, unbound: whom to bind
it to, and where, is the operator's decision about people. A comment names the namespaces.
The delivery profile's Role is rendered the same way.

## Alternatives considered

- **Token passthrough**: sending the caller's own bearer token to the API server. The MCP
  authorization specification forbids a server from passing on the token it received: that
  token's audience is Opsward, and an API server accepting it would accept tokens meant for
  any service of the same issuer. Impersonation gives the cluster the person's name without
  that, and it is no longer on the roadmap.
- **Token exchange** (RFC 8693) for a token whose audience is the cluster: per-person
  credentials without `impersonate`, but it needs an identity provider that exchanges tokens.
  Revisit if one is asked for.
- **Rendering per-person RBAC from policy.** The cluster's RBAC would mirror Opsward's
  policy, which is then decided twice; impersonation is for teams whose cluster already says
  what each person may do.
