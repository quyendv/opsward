# AGENTS.md

Context for AI coding agents and human contributors working in this repository.

## What this is

Opsward is a guarded gateway that lets AI agents operate infrastructure (Kubernetes,
GitOps, observability backends, CI) within the permissions an operator grants. Its value
is not raw API access, which many MCP servers already offer, but **typed operational tools,
per-principal policy, audit, redaction, and a write path that proposes changes instead of
making them**.

Status: **pre-alpha.** Nothing is stable. One tool works end to end over MCP stdio.
Read the design notes in [docs/design/](docs/design/) before adding a package:
[0001](docs/design/0001-architecture.md) for the architecture,
[0002](docs/design/0002-identity-policy-storage.md) for identity, policy and storage.

## Layout

```
cmd/opsward/                 CLI: serve, validate, audit verify, version
internal/core/               vocabulary shared by engine, policy and audit
internal/engine/             the pipeline: validate, authorize, run, redact, audit
internal/policy/             roles, bindings, CEL conditions
internal/audit/              hash-chained records, stream, verification, retention
internal/store/              persistence interface; sqlite/ implements it
internal/redact/             credential-shaped content removal
internal/capability/         backend-neutral interfaces tools depend on
internal/provider/<name>/    backend implementations (kubernetes)
internal/tool/               the agent's vocabulary
internal/adapter/<protocol>/ the only place protocol SDKs appear (mcp)
internal/app/                wiring from configuration
examples/                    configuration examples
```

## Commands

All tool versions are pinned in `.mise.toml`; run everything through mise.

```sh
mise run setup    # once after cloning: tools + pre-commit and pre-push hooks
mise run fmt      # gofumpt + goimports via golangci-lint
mise run check    # what CI runs: tidy, lint, test (-race), govulncheck, gitleaks history
mise run build    # ./bin/opsward
```

The Go version appears in `.mise.toml`, `go.mod` (minimum) and `Dockerfile`; bump them together.

## Invariants

These are design decisions, not style preferences. Changing one needs a design note in
`docs/design/`, not just a code change.

1. **The engine is client-agnostic.** Only `internal/adapter/<protocol>` may import a protocol
   SDK. Enforced by the `mcp-at-the-edge` depguard rule.
2. **No free-form command channel.** No tool accepts a shell command, a kubectl argument list,
   or an arbitrary API verb or path. `os/exec` is banned repository-wide (`no-exec` rule).
   Read-only query languages (PromQL, LogQL, TraceQL) are allowed because they cannot mutate.
3. **Every tool declares a risk class** (`read`, `write`, `destructive`) and takes typed scope
   arguments (cluster, namespace, resource), so policy can evaluate a call without parsing it.
4. **One authorization point.** Every invocation goes through a single
   `authorize(principal, tool, args)` and emits an audit record, denials included. Single-user
   mode still has a principal.
5. **Fail closed.** An unknown tool, a missing policy, or an unparseable config denies.
6. **Secret values never reach a model.** Output passes through redaction before it leaves the
   engine; tools do not read secret payloads at all where the backend allows that.
7. **Prefer proposing over mutating.** The default write path is a pull request against the
   declarative source of truth. Direct mutations are narrow, named verbs behind approval.
8. **Credentials are scoped per profile**, never one administrative credential for everything.
   The backend's own authorization is the real boundary; Opsward's policy is a second layer.
   Opsward never creates credentials or grants permissions in a backend.
9. **Tools are named for what they do, not for the backend.** `list_workload_logs`, not
   `loki_query`; swapping a backend must not rename a tool.

## Conventions

- **This repository is public-bound and vendor-neutral.** No organization names, hostnames,
  cluster names, internal URLs or incident details in code, tests, docs or commit messages.
  Examples use `example.com`, `cluster-a`, `team-a`. Teach the shape of a value, not a value.
- English for everything written to files.
- Maintainers list private terms (organization names, hostnames, addresses) as regexes in
  `.local/private-terms.txt`; `scripts/private-terms.sh` blocks them in staged changes,
  commit messages, identities and unpushed commits. Without the file the check is skipped;
  `git config opsward.privateTermsRequired true` makes a missing file an error.
- `docs/` holds decided project information only. Discussion, open questions and drafts live
  in `.local/`, which is git-ignored; promote a conclusion into `docs/` once it is decided.
- Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`, `test:`, `refactor:`).
  Branches use the same prefixes: `feat/…`, `fix/…`.
- Errors: wrap with `fmt.Errorf("…: %w", err)`; logging uses `log/slog`.
- Comment what would break if the comment were deleted; rationale belongs in `docs/design/`.
- Update `CHANGELOG.md` (Keep a Changelog) under `Unreleased` for user-visible changes.

## Verifying a gate

A check that passes proves nothing until you have seen it fail. When adding or changing a
lint rule, hook, policy or test that is meant to block something, plant a probe that violates
**exactly that one** rule (and nothing else, so no other check can fail first), watch it
block for the stated reason, then remove the probe. For secret scanning, also confirm the
"N commits scanned" count is the one you expected, not just the exit code.
