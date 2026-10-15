# Contributing

Thanks for your interest. Opsward is pre-alpha; design discussion in issues is as welcome
as code.

## Setup

```sh
mise run setup   # installs pinned tools and the pre-commit and pre-push hooks
```

Before pushing, `mise run check` must pass. It is exactly what CI runs.

The PostgreSQL store tests run when `OPSWARD_TEST_POSTGRES_DSN` names a database in which
they may create and drop schemas; without it they are skipped locally, and CI provides one:

```sh
docker run -d --name opsward-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_USER=opsward -e POSTGRES_PASSWORD=opsward -e POSTGRES_DB=opsward postgres:18-alpine
export OPSWARD_TEST_POSTGRES_DSN='postgres://opsward:opsward@127.0.0.1:55432/opsward?sslmode=disable'
```

## Pull requests

- Read the invariants in [AGENTS.md](AGENTS.md). A change that relaxes one needs a design
  note in `docs/design/` and will be discussed before it is reviewed as code.
- Use Conventional Commits; branch names use the same prefixes (`feat/…`, `fix/…`).
- Add a `CHANGELOG.md` entry under `Unreleased` for user-visible changes.
- Pull requests are squash-merged by default, so the PR title becomes the commit on `main`
  and must follow Conventional Commits. A large PR whose commits each tell part of the story
  may be merged with a merge commit instead.
- Builtin knowledge documents (`internal/knowledge/builtin/`) must be generic. Lessons
  specific to one organization belong in that organization's own knowledge path.
- Keep examples generic: `example.com`, `cluster-a`, never real hostnames or organization names.
- Never commit credentials, kubeconfigs or real tokens, even expired ones. The hooks scan for
  them, but they are a safety net, not a process.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not use public issues.
