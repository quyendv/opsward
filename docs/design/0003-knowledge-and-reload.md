# 0003: Knowledge packs, workload events and logs, configuration reload

- Status: accepted
- Date: 2026-10-15
- Extends: [0001](0001-architecture.md), [0002](0002-identity-policy-storage.md)

## Summary

Milestone 2 of v0.1 adds:

1. **Knowledge packs** that agents can search and that attach themselves to tool results.
2. **Two workload tools**, `list_workload_events` and `list_workload_logs`.
3. **Configuration reload** without a restart, for everything except identity, storage and
   audit.

Structural redaction of Kubernetes objects is deferred until a tool returns raw objects. The
tools in v0.1 return reduced structures, and their free text (event messages, log lines)
passes through pattern redaction.

## Knowledge packs

### Format

A pack is a directory of Markdown files. Each file starts with YAML frontmatter:

```markdown
---
id: crashloop-backoff
title: A container keeps crashing and restarting
severity: warning
summary: Read the previous container's logs; the current one has usually not failed yet.
applies-to:
  tools: [get_workload_status]
  when: 'result.pods.exists(p, has(p.waiting) && p.waiting == "CrashLoopBackOff")'
---
Body in Markdown ...
```

| Field | Meaning |
|---|---|
| `id` | required; lowercase letters, digits and dashes; unique across every loaded pack |
| `title` | required |
| `severity` | `info` (default), `warning` or `critical` |
| `summary` | the text attached to tool results; defaults to the body's first paragraph |
| `applies-to.tools` | tools whose results the document is attached to |
| `applies-to.when` | optional CEL condition that must also hold |

A document without `applies-to` can be searched but is never attached.

### Conditions

`when` is CEL, as in policy, with these variables:

| Variable | Content |
|---|---|
| `tool` | `name`, `risk` |
| `scope` | `environment`, `namespace`, `kind`, `name` |
| `args` | the call's validated arguments |
| `result` | the tool's result after redaction, as JSON values |

Conditions are compiled and type-checked when packs are loaded. A condition that fails to
evaluate does not attach its document. Attachment is advice, so failing open is acceptable
here, unlike policy.

Matching on the result rather than on the request is what lets a document react to what was
found, such as a pod in `CrashLoopBackOff`, instead of only to what was asked.

### Use

- **Searched.** `search_knowledge` ranks documents by query terms in the title, summary and
  body. `get_knowledge` returns one document in full.
- **Attached.** After a tool runs, every document whose `applies-to` matches is attached to the
  result as a note: `id`, `title`, `severity`, `summary`. The MCP adapter sends the notes as a
  separate content block after the result.

Both knowledge tools are `read` and **not bound to an environment**. They still go through
authorization, with an empty scope. A binding restricted to environments or namespaces does
not match them, so granting them needs a binding without those restrictions.

### Sources

```yaml
knowledge:
  builtin: true            # the generic pack embedded in the binary (default true)
  paths: [/etc/opsward/knowledge]
```

Files are read recursively. A duplicate `id`, an invalid frontmatter field or a condition that
does not compile is an error. It prevents start-up; on reload the previous packs stay in force.
Organization-specific packs live in the operator's repository and are loaded by path.

## Workload events and logs

**`list_workload_events`** returns the Kubernetes events of the workload, of the ReplicaSets a
Deployment owns, and of the workload's pods. The pods matter most: `FailedScheduling`,
`ImagePullBackOff` and failed probes are reported on the pod, not the Deployment. Events come
newest first, 50 by default, at most 200.

**`list_workload_logs`** returns the last lines of each container of the workload's pods:

| Argument | Default | Limit |
|---|---|---|
| `container` | every container | |
| `tailLines` | 100 | 1000 |
| `previous` | false: the running container; true: the one before the last restart | |

Pods are read in name order, at most 10, and the response says how many were left out. Each
container's output is capped at 256 KiB. A container that cannot be read, such as a `previous`
request for a container that never restarted, gets an error entry; the other containers are
still returned.

Both tools are `read`, resolve the workload's pods with the call's own profile, and need
`get` on `pods/log` and `list` on `events` in that profile's role.

## Configuration reload

Opsward checks its configuration file every 10 seconds and on `SIGHUP`. A changed file is
loaded completely: configuration, policy, environments, providers and knowledge packs. Only
when all of it is valid is anything replaced. Providers and packs are swapped first, then
policy and environments together; a call already running during that instant may see the new
providers with the previous policy, but never a partially loaded configuration. Content that
was rejected is not retried until the file changes again.

| Section | On reload |
|---|---|
| `policy`, `environments`, `providers`, `knowledge` | replaced |
| `identity`, `storage`, `audit` | must be unchanged; a change rejects the whole reload and needs a restart |

A rejected reload is logged with its reason and the previous configuration stays in force. A
Kubernetes client whose profile changed is rebuilt on its next use. Calls already running
finish under the configuration they started with.

The `opsward_config_last_reload_successful` metric in 0002 arrives with the HTTP adapter.
Until then the log is the only signal.
