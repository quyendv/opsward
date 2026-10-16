# 0011: Running in a cluster

- Status: accepted
- Date: 2026-10-16
- Extends: [0005](0005-remote-access.md), [0009](0009-rbac-render.md)

## Summary

Opsward ships as a binary and a container image, but nothing says how to run the image for
a team: where the configuration lives, how a credential profile becomes a credential in the
pod, where SQLite keeps its file. A Helm chart, `charts/opsward`, answers that for remote
mode. It is published with each release as `oci://ghcr.io/quyendv/charts/opsward`, at the
release's version.

## What the chart runs

- One Deployment of the image in remote mode, behind a Service on port 8080 (`/mcp`,
  `/healthz`, `/readyz`), with an optional Ingress. The chart refuses a configuration
  whose `identity.mode` is not `remote`: stdio has no place in a cluster.
- The configuration is the `config` value, written to a ConfigMap and mounted as a
  directory, so an edit reaches the running process and applies without a restart (0003).
  It references credentials by file; they are mounted from Secrets the operator names.
- SQLite needs one replica and a volume (`persistence`, mounted at `/var/lib/opsward`); more
  than one replica needs Postgres, and the chart refuses `replicas > 1` otherwise.
- The pod runs as non-root, with a read-only root filesystem, no capabilities and the
  default seccomp profile. Its own ServiceAccount has no token mounted: Opsward never uses
  the pod's identity (`FromKubeconfig` refuses in-cluster fallback).

## Profiles in the cluster Opsward runs in

For each name in `localProfiles` the chart creates a ServiceAccount `opsward-<profile>`, a
token Secret for it, and a kubeconfig at `/etc/opsward/kubeconfig/<profile>/kubeconfig`
that reaches the API server in-cluster with that token. The configuration's profile then
names that path. These ServiceAccounts are exactly the default subjects of `opsward rbac
render` with `--service-account-namespace` set to the release namespace, so the operator
renders, reviews and applies the RBAC; the chart can carry the reviewed output
(`rbac.rendered`) so that it is installed with the release. The chart never derives RBAC by
itself: what each profile may do is decided by policy, which only Opsward reads.

Profiles for other clusters reference kubeconfigs the operator provides as Secrets
(`extraSecretMounts`), like any other credential file.

## Alternatives considered

- **Projected tokens for every profile.** A projected token belongs to the pod's own
  ServiceAccount; a pod has one, and profiles need several identities.
- **Plain manifests or Kustomize.** Fewer moving parts, but every installation would edit
  the same paths and names by hand; a chart keeps them consistent with the conventions
  above.
