# ADR 0005: Webhook forwarder lifecycle is local-only

`looperd` may persist and reconcile records for `gh webhook forward` subprocesses that it spawned. The persisted row identifies a local process by repo, PID, process start time, executable path, endpoint, events, and daemon instance.

The authority for adopting or signalling a process is the local identity gate: PID plus process start time plus executable and command shape. Looper does not infer ownership from GitHub hook state and does not call the GitHub hooks API.

Rejected alternatives:

- deleting or adopting remote GitHub hooks directly, because `gh webhook forward` owns that lifecycle;
- enumerating unrelated `gh` processes on first upgrade boot, because Looper has no prior local record proving it spawned them;
- keeping forwarders alive during a graceful restart, because stop and restart are indistinguishable without a future re-exec design.

If local lifecycle reconciliation fails, webhook latency falls back to the existing polling interval. Correctness remains with the poller; this lifecycle only improves delivery latency and operator diagnostics.
