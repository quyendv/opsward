---
id: image-pull-backoff
title: A pod cannot pull its container image
severity: warning
summary: >-
  The node cannot fetch the image. Check the image name and tag for a typo, whether the
  tag exists in the registry, and whether the pod has pull credentials for a private
  registry; the pod's events carry the registry's own error.
applies-to:
  tools: [get_workload_status]
  when: 'result.pods.exists(p, has(p.waiting) && p.waiting in ["ErrImagePull", "ImagePullBackOff", "InvalidImageName"])'
---
`ErrImagePull` is the first failure; `ImagePullBackOff` is Kubernetes waiting before it
tries again. `InvalidImageName` means the reference cannot be parsed at all.

Read the exact error in `list_workload_events`. Common causes:

- **The tag does not exist.** A CI pipeline that has not pushed yet, or a tag that was
  deleted or overwritten.
- **Authentication.** A private registry needs `imagePullSecrets` on the pod or its
  ServiceAccount, and the secret must be valid for that registry host.
- **Rate limits or reachability.** Public registries limit anonymous pulls; a node without
  egress to the registry fails the same way for every image.
- **Architecture.** An image built for one CPU architecture fails on nodes of another with
  a "no matching manifest" error.
