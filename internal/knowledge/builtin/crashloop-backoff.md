---
id: crashloop-backoff
title: A container keeps crashing and restarting (CrashLoopBackOff)
severity: warning
summary: >-
  Read the logs of the previous container instance (list_workload_logs with
  previous=true); the running one has usually not failed yet. The pod's lastTerminated
  shows how it ended (OOMKilled, or Error with an exit code).
applies-to:
  tools: [get_workload_status]
  when: 'result.pods.exists(p, has(p.waiting) && p.waiting.endsWith("CrashLoopBackOff"))'
---
Kubernetes restarts a failing container with an exponential back-off of up to five
minutes, and reports the wait as `CrashLoopBackOff`. The state says that the container
keeps exiting, not why.

1. **Read the crash, not the restart.** The current instance may still be starting.
   Read the previous instance's logs: `list_workload_logs` with `previous: true`.
2. **Separate the causes.** `lastTerminated` in `get_workload_status` says how the last
   instance ended. `OOMKilled` (exit code 137) means the memory limit; `Error` with exit code
   1 or 2 is usually the program itself, which logs why before it exits. A process killed
   from outside without OOM is often a failed liveness probe: look in
   `list_workload_events`.
   `Init:CrashLoopBackOff` means an init container, such as a migration, is the one
   failing: read it with `list_workload_logs` and `container` set to its name.
3. **Compare with the last change.** A crash loop that starts with a rollout points at the
   new image or configuration; one that starts without a rollout points at a dependency the
   container needs at start-up.
4. **Be careful with liveness probes.** A probe that checks a dependency, or that times out
   during a slow start, turns a temporary problem into a restart loop.
