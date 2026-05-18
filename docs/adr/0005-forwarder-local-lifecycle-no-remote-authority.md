# ADR 0005: Webhook forwarder lifecycle is local-only

`looperd` may persist and reconcile records for `gh webhook forward` subprocesses that it spawned. The persisted row identifies a local process by repo, PID, process start time, executable path, endpoint, events, and daemon instance.

The authority for adopting or signalling a process is the local identity gate: PID plus process start time plus executable and command shape. `looperd` does not infer ownership from GitHub hook state and does not call the GitHub hooks API as part of startup, reconcile, retry, or latch handling.

An explicit user-invoked CLI cleanup command may inspect and delete GitHub CLI `cli` hooks that point at `https://webhook-forwarder.github.com/hook`, but that command is a manual remediation path, not daemon authority. It must stay dry-run by default, require an explicit target repo, and require explicit user confirmation before deletion.

Rejected alternatives:

- automatic daemon deletion or adoption of remote GitHub hooks, because `gh webhook forward` owns that lifecycle and Looper should not mutate remote hook state on inference alone;
- enumerating unrelated `gh` processes on first upgrade boot, because Looper has no prior local record proving it spawned them;
- keeping forwarders alive during a graceful restart, because stop and restart are indistinguishable without a future re-exec design.

If local lifecycle reconciliation fails, webhook latency falls back to the existing polling interval. Correctness remains with the poller; this lifecycle only improves delivery latency and operator diagnostics.
