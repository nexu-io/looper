# Reviewer subprocess sandbox with nono

Looper can optionally run **Reviewer agent subprocesses** through [nono](https://github.com/nolabs-ai/nono). Planner, Worker, Fixer and Coordinator keep their existing execution paths. Reviewer thread-resolution/classification subprocesses use the same sandbox as review generation.

This is a file-only, global operator setting, disabled when omitted. Project-level overrides are rejected. Changing it requires a daemon restart. Install and pin nono separately; the tested version is **v0.81.0** (macOS ARM64 and Linux ARM64). Verify upstream release checksums. No root is required. Linux needs working Landlock and seccomp user notification for pathname Unix socket mediation; run the enforcement test on the deployment host rather than relying on a kernel version string.

```toml
[roles.reviewer.nono]
command = "/opt/nono/bin/nono" # optional; defaults to nono on the daemon's PATH
profile = "/etc/looper/reviewer-nono.json" # required absolute file path
```

Equivalent JSON:

```json
{"roles":{"reviewer":{"nono":{"command":"/opt/nono/bin/nono","profile":"/etc/looper/reviewer-nono.json"}}}}
```

Remove the `nono` object to disable it, then restart. Missing executables, missing profiles and sandbox startup failures fail the execution; Looper never retries outside nono. Native-resume retries also pass through nono. The original vendor command/arguments are wrapped **after** vendor argument generation; do not set `agent.params.command` to nono.

## Policy authority and scope

The daemon operator owns the profile, binary, runtime homes and permitted endpoints. Store policy and executables outside repository worktrees and outside all agent-writable paths, including writable ancestors. Repository instructions and agent output are not policy authority. Pin inherited profiles/packs too; changing the profile file affects subsequent launches even without changing the Looper config.

Looper adds only:

- Read access to the current Reviewer working directory.
- Read access to per-run hosting shims and the trusted Looper CLI, when bot hosting is active.
- Exact per-run hosting/review Unix socket connection grants. It never grants the socket parent directory.

The operator supplies system/runtime dependencies, shared Git common-directory reads, model credentials, session/cache writes and model network endpoints. A Git worktree's `.git` file points outside the worktree: grant its shared Git common directory **read-only**, not the whole Looper data root. Reviewers that need temporary build output must use a dedicated writable scratch directory or an explicitly reviewed writable-worktree policy.

The boundary covers commands launched by the Reviewer agent, including repository scripts it invokes. It does **not** cover looperd, nono's trusted supervisor/proxy, daemon-side repository preparation, publication, or validation subprocesses launched separately by looperd. Existing hosting brokers remain outside the sandbox and enforce their own action scope. Use a bot hosting identity so raw forge credentials remain in the broker; filesystem isolation does not remove secrets deliberately placed in the child environment. Model credentials visible to the model client are also visible to its subprocesses.

## Build a narrow profile

nono v0.81.0 merges custom profiles with built-in defaults. An empty `groups.include` does **not** remove those defaults. In particular, macOS defaults include broad `/private` reads and temporary-directory writes, and Linux defaults include shared `/tmp` writes. Exclude those groups before adding narrow grants. Do not grant all of `$HOME`, `/tmp`, `/private`, the Looper data root or the whole agent home just to silence a denial.

This starting point intentionally needs operator-specific paths and domains. Replace paths with existing directories/files and add only the runtime libraries/configuration your installed agent needs:

```json
{
  "meta": {"name": "looper-reviewer"},
  "groups": {
    "include": [],
    "exclude": ["system_read_macos", "system_write_macos", "system_write_linux"]
  },
  "workdir": {"access": "read"},
  "filesystem": {
    "read": ["/bin", "/usr/bin", "/usr/lib", "/etc/ssl", "/opt/reviewer/runtime", "/srv/reviewer/repository.git"],
    "read_file": ["/etc/resolv.conf", "/opt/reviewer/model-config.json"],
    "allow": ["/srv/reviewer/sessions", "/srv/reviewer/cache", "/srv/reviewer/tmp"],
    "write_file": ["/dev/null"]
  },
  "network": {"allow_domain": ["api.model-provider.example"]},
  "security": {"capability_elevation": false},
  "linux": {"af_unix_mediation": "pathname"}
}
```

On macOS, add the required system-library reads (for example `/System/Library`) without restoring the broad `system_read_macos` group. Configure the agent's session/cache/temp locations to match the dedicated writable grants. Each writable directory is fully trusted by that agent and should contain no host credentials or executable paths used by other services. Keep the policy file outside it.

Set `security.capability_elevation` to **false**, including inherited policy, and configure no interactive approvals, hooks or runtime policy expansion. Looper invokes nono silently, with rollback and the TUI startup watchdog disabled, so structured stdout/stderr and exit status remain usable by the executor. It strips `NONO_*`, `LD_*` and `DYLD_*` environment overrides before launching nono. Other environment variables retain the existing vendor/hosting behavior; review them for secrets and runtime preload/configuration features.

Network is **allowed by default in nono**. Supply a model endpoint allowlist to activate proxy filtering, or `"network":{"block":true}` for an offline probe. A permitted endpoint remains a possible data destination. Do not broadly permit forge/cloud endpoints to the model client if it only needs Looper's narrow hosting broker.

On Linux, `linux.af_unix_mediation: "pathname"` is required for exact Unix socket enforcement. Ordinary filesystem grants alone do not enforce the intended Unix socket allowlist. Keep SSH agents, Docker/container sockets, databases and unrelated services outside that allowlist. Do not use startup-only `commands.allow`/`commands.deny` as a child-command security boundary.

## Validate before enabling a shared reviewer

The default tests use fake commands and need no nono installation. Run the opt-in test with a pinned binary on the deployment host:

```sh
LOOPER_TEST_NONO=/opt/nono/bin/nono go test ./internal/agent -run '^TestNonoLive$' -count=1 -v
```

It uses synthetic files, local TCP/Unix listeners and a mock agent; it invokes no provider and performs no forge mutations. It checks allowed source reads/scratch writes, denied secret reads/source writes/TCP connections, exact Unix socket access, separate stdout/stderr, structured results, and cancellation/timeout cleanup of descendants. It creates and cleans a temporary fixture under `/tmp` to keep Unix socket paths short.

Then test the **actual installed agent and final operator profile** in a disposable repository, including shared Git reads, configured model connectivity, hosting broker reads, denied credentials/sockets, and stop/timeout cleanup. The synthetic test validates the integration and OS primitives; it does not certify a broader operator profile or exercise a real model client. Enabling this setting for a shared production instance is a separate operational rollout.
