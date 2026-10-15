---
id: oom-killed
title: A container was killed for exceeding its memory limit (OOMKilled)
severity: warning
summary: >-
  The container used more memory than its limit and the kernel killed it (exit code 137).
  Compare the limit with the workload's real peak usage before raising it; a leak only
  takes longer to hit a higher limit.
applies-to:
  tools: [get_workload_status, diagnose_workload]
  when: '(has(result.status) ? result.status : result).pods.exists(p, has(p.lastTerminated) && p.lastTerminated.reason == "OOMKilled")'
---
`OOMKilled` is reported in the container's last termination state, not as an event, so it
is easy to miss: the pod only shows restarts.

- **Limit versus usage.** The kill happens at the container's memory limit, regardless of
  free memory on the node. Look at the usage just before the kill, not the average.
- **Runtimes that size themselves.** JVMs, Node.js and similar runtimes pick heap sizes from
  the visible memory; a heap setting close to the limit leaves no room for the rest of the
  process.
- **Leak or load.** Usage that climbs steadily until each kill points at a leak; raising the
  limit postpones it. Usage that spikes with traffic points at the limit being too low for
  real load.
- **Node pressure.** A pod evicted because the node ran out of memory is reported as an
  eviction, not as `OOMKilled`; that is a scheduling and capacity problem instead.
