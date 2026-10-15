---
id: crashloop-backoff
title: A container keeps crashing and restarting (CrashLoopBackOff)
severity: warning
summary: >-
  Read the logs of the previous container instance (list_workload_logs with
  previous=true); the running one has usually not failed yet. Then check the exit
  reason, recent configuration changes and the liveness probe.
applies-to:
  tools: [get_workload_status]
  when: 'result.pods.exists(p, has(p.waiting) && p.waiting == "CrashLoopBackOff")'
---
Kubernetes restarts a failing container with an exponential back-off of up to five
minutes, and reports the wait as `CrashLoopBackOff`. The state says that the container
keeps exiting, not why.

1. **Read the crash, not the restart.** The current instance may still be starting.
   Read the previous instance's logs: `list_workload_logs` with `previous: true`.
2. **Separate the causes.** A process that exits on its own (configuration error, missing
   dependency, failed migration) logs why before it exits. A process killed from outside
   usually does not: look for a failed liveness probe in `list_workload_events`, or an
   out-of-memory kill.
3. **Compare with the last change.** A crash loop that starts with a rollout points at the
   new image or configuration; one that starts without a rollout points at a dependency the
   container needs at start-up.
4. **Be careful with liveness probes.** A probe that checks a dependency, or that times out
   during a slow start, turns a temporary problem into a restart loop.
