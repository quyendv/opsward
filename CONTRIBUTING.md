# Contributing

Thanks for your interest. Opsward is pre-alpha; design discussion in issues is as welcome
as code.

## Setup

```sh
mise run setup   # installs pinned tools and the pre-commit and pre-push hooks
```

Before pushing, `mise run check` must pass. It is exactly what CI runs.

## Pull requests

- Read the invariants in [AGENTS.md](AGENTS.md). A change that relaxes one needs a design
  note in `docs/design/` and will be discussed before it is reviewed as code.
- Use Conventional Commits; branch names use the same prefixes (`feat/…`, `fix/…`).
- Add a `CHANGELOG.md` entry under `Unreleased` for user-visible changes.
- Keep examples generic: `example.com`, `cluster-a`, never real hostnames or organization names.
- Never commit credentials, kubeconfigs or real tokens, even expired ones. The hooks scan for
  them, but they are a safety net, not a process.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not use public issues.
