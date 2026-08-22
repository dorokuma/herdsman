## 0.6.1

- Delivery pipeline: unified the deliverable-event predicate across pending discovery, acknowledgement, and publication so filtered events can no longer pin the queue (ddbc5ca, 49b6218).
- Wake correctness: dropped empty wake turns, isolated interactive Pi observers from dispatched roles, and stopped aborting in-flight turns on transient disconnects (53c3513, 302c837, 97a54bf).
- Observation chain: rebuild the Herdr client with exponential backoff on subscription failure, discard stale session refreshes via a mutation epoch, and harden session-path classification (3b1e922).
- Security and process hardening: private observability sockets, non-preemptive scope claims, bounded JSON-lines frames, exclusive daemon PID startup, readiness polling, SIGKILL escalation, SQLite WAL/busy timeout, and log rotation (adff7ef).
- Daemon status now reports a reachable daemon without a PID file as running and exposes pidFileMissing (5a1f78d).
- Test suite grew from 240 to 255 cases across 33 files, covering disconnect, frame-limit, path-classification, and acknowledgement-cursor regressions (5f47567).

## 0.6.0

Forked from @ryonakae/shepherd at v0.5.1 (dfdd3a2). Previous history is inherited from upstream.
