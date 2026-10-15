---
id: failed-scheduling
title: The scheduler cannot place a pod (FailedScheduling)
severity: warning
summary: >-
  No node satisfies the pod's requirements. The event message counts the nodes rejected
  for each reason: insufficient CPU or memory, taints, node selectors or affinity, or a
  volume bound to another zone or node.
applies-to:
  tools: [list_workload_events]
  when: 'result.exists(e, e.reason == "FailedScheduling")'
---
A `FailedScheduling` message such as `0/4 nodes are available: 2 Insufficient memory,
2 node(s) had untolerated taint` lists every reason and how many nodes it excluded.

- **Insufficient CPU or memory** is about requests, not usage: the sum of requests on a
  node cannot exceed what it can allocate. Lower the requests or add capacity.
- **Taints** keep pods off nodes unless the pod tolerates them; control-plane and
  dedicated nodes are tainted on purpose.
- **Node selectors and affinity** that name labels no node has never match.
- **Volume node affinity**: a volume bound to one zone or node limits the pod to it. A
  pod whose volume lives on a node that is full or gone stays pending.
- **Anti-affinity** with as many replicas as eligible nodes leaves the extra replicas
  pending by design.
