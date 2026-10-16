# 0008: Proposing changes

- Status: accepted
- Date: 2026-10-16
- Extends: [0001](0001-architecture.md) (Change model), [0007](0007-operate-and-approvals.md)

## Summary

Most changes belong in the declarative source a reconciler applies, not in the cluster:
0007 refuses to scale a workload Argo CD manages for that reason. This note lets an agent
propose such a change: Opsward finds the files that declare the workload, applies the
agent's edits to them, and returns the diff (`suggest`) or opens a pull request
(`pull-request`). A person reviews and merges; whatever applies merged changes is outside
Opsward.

| Tool | Risk | What it does |
|---|---|---|
| `resolve_source` | read | the repository, revision and files that declare a workload, and the content of up to 10 of them |
| `propose_change` | read under `suggest`, write under `pull-request` | edits to those files, as a diff or a pull request |

## Resolving the source

Resolvers are tried in order; the first answer wins (0001):

1. **Argo CD.** The workload's `argocd.argoproj.io/tracking-id` annotation (or, with
   `labelTracking`, its `app.kubernetes.io/instance` label) names an Application, read from
   the cluster and namespace where Argo CD keeps them. As Argo CD itself requires, the
   tracking id must name the workload (`<app>:apps/<kind>:<namespace>/<name>`) and the
   Application must deploy to the workload's namespace; otherwise the workload does not
   resolve, so it cannot borrow another Application's source. Each of the Application's sources gives
   a repository, a revision and a path; a Helm source adds its value files, a source that
   is only a `ref` provides the repository other sources' `$ref/…` value files point into.
2. **An annotation** on the workload, `opsward/source: <repository>//<path>[@<revision>]`,
   under the keys in `sourceResolution.annotations`.
3. **A static map** in the configuration, by environment, namespace and workload.

A source is a list of locations: a repository, a revision, and a path that is a file or a
directory (`.` is the whole repository). Paths are cleaned and must stay inside the
repository; revisions must be valid refs. Every revision is resolved once to a commit,
which is what files are read, listed and proposed from. Reading the Application needs `get` on `applications.argoproj.io` in that
namespace for the delivery profile of that cluster (`reader` unless `profile` names
another). A workload is read with the call's own profile, like every other tool.

```yaml
providers:
  delivery:
    type: argocd
    cluster: cluster-a        # where the Application objects live
    namespace: argocd         # default
    labelTracking: false      # default: the tracking-id annotation
  forges:
    - name: github
      type: github            # github, gitea (also Forgejo) or gitlab
      url: https://api.github.com
      hosts: [github.com]     # repository URLs on these hosts use this forge
      token: { file: /etc/opsward/github-token }
      repositories: ["example-org/gitops"]   # the only ones Opsward reads or writes

environments:
  - name: staging
    cluster: cluster-a
    change: { strategy: pull-request }

sourceResolution:
  annotations: [opsward/source]
  static:
    - { environment: staging, namespace: shop, name: api, source: "github:example-org/gitops//apps/api@main" }
```

A repository is reached only if a forge lists it: an Application pointing elsewhere
resolves, but cannot be read or written. Within listed repositories, whoever can annotate
a workload chooses its source: an operator who does not trust workload annotations
leaves `sourceResolution.annotations` empty and relies on Argo CD and the static map.

## Proposing

```json
{ "environment": "staging", "namespace": "shop", "kind": "Deployment", "name": "api",
  "title": "Scale api to 4 replicas",
  "description": "Load doubled since the campaign started.",
  "edits": [ { "path": "apps/api/values.yaml", "old": "replicaCount: 2", "new": "replicaCount: 4" } ] }
```

- The call is authorized on the **workload**, like every other tool: policy needs no notion
  of files.
- Every edit must be to a file inside the workload's resolved source; anything else is
  refused. Edits replace text: `old` must occur exactly once in the file at the source's
  revision, which keeps an edit precise and makes a stale one fail rather than apply
  elsewhere. At most 10 files and 1 MiB per file.
- The diff is computed by Opsward, unified, and passes through redaction before it is
  returned. In files and diffs shown to the caller, the values under `data` and
  `stringData` of `kind: Secret` manifests are blanked; the proposal itself keeps them.

| Strategy | Result | Risk |
|---|---|---|
| `suggest` (default) | the diff; nothing is written | read |
| `pull-request` | a branch `opsward/<id>`, one commit, a pull request against the source's revision; the URL and the diff | write |

`<id>` is the id of the call's intent audit record, written before the pull request is
opened; the pull request names it with the principal. A source pinned to a commit cannot
take a pull request and is refused.

A tool's risk is fixed, except here: `propose_change` takes its risk from the environment's
strategy before authorization, so a role with `risk: [read]` can suggest everywhere and
open pull requests nowhere. The pull request names the principal who proposed it and the
audit record id; its commit is authored by the forge credential. A pull request is
review, so policy usually allows it outright; `approve` still works.

If nothing resolves, `propose_change` proposes nothing and says why: under `suggest` as its
answer, under `pull-request` as a refusal before any approval is requested. It never falls
forward to a direct change.

## Forges

One interface, three implementations on each forge's REST API:

| Operation | GitHub | Gitea, Forgejo | GitLab |
|---|---|---|---|
| read a file at a revision | contents API | contents API | repository files API |
| list a directory, recursively | git trees | git trees | repository tree |
| branch, one commit, pull request | git trees, commits, refs; pulls | change files (new branch); pulls | commits with actions (new branch); merge requests |

The token is referenced, never inline, and each forge has an explicit repository list:
Opsward does not write to a repository nobody named, whatever an Application says.

## Alternatives considered

- **Typed changes** (`replicas`, `image`) mapped onto the source. Clean for plain
  manifests, but a Helm chart's values have no fixed relation to the fields they set, so
  most GitOps repositories could not be edited.
- **Unified diffs from the agent.** Compact, but agents produce diffs that do not apply,
  and a diff against a stale file can land in the wrong place; exact text replacement
  fails loudly instead.
- **Cloning repositories.** Works with any Git host, but needs disk, credentials for Git
  itself and a Git implementation; the forges' APIs cover a few files per change.
