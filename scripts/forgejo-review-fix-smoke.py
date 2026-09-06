#!/usr/bin/env python3
"""Opt-in live Forgejo/Codex lifecycle check using an existing tea login.

Creates and closes one test PR; uses only its own branch, label, daemon and DB.
Requires go, git, tea, python3 and an authenticated codex. Run --help for usage.
"""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[1]


def execute(argv, *, cwd=None, env=None, timeout=300):
    result = subprocess.run([str(a) for a in argv], cwd=cwd, env=env,
                            text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{argv[0]} exited {result.returncode}: {result.stderr[-1600:]}")
    return result.stdout


def save(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")


class Smoke:
    def __init__(self, args):
        self.args = args
        self.run_id = time.strftime("%Y%m%d-%H%M%S", time.gmtime()) + f"-{os.getpid()}"
        self.branch = "looper-smoke-" + self.run_id
        self.label_name = "looper-smoke:" + self.run_id
        self.out = Path(args.output or ROOT / "dist" / self.branch).resolve()
        self.out.mkdir(parents=True, exist_ok=False)
        self.checkout = self.out / "repo"
        self.bin_dir = self.out / "bin"
        self.bin_dir.mkdir()
        # Keep vendor/tea authentication in the current HOME. All Looper runtime
        # paths are explicit; inherited Looper overrides must not select another DB.
        self.env = {k: v for k, v in os.environ.items() if not k.startswith("LOOPER_")}
        self.env.update(GIT_TERMINAL_PROMPT="0", GCM_INTERACTIVE="Never")
        self.pr_number = None
        self.label_id = None
        self.label_attempted = False
        self.pr_attempted = False
        self.pushed = False
        self.proc = None
        self.session_id = None
        self.log = None
        self.cfg = None
        self.default_branch = None
        self.initial_default_sha = None
        self.evidence = {"run_id": self.run_id, "repo": args.repo, "base_url": args.base_url,
                         "branch": self.branch, "label_name": self.label_name}

    def tea(self, path, method="GET", body=None, *, missing_ok=False, include_headers=False):
        argv = ["tea", "api", "--login", self.args.tea_login, "-i", "-X", method, path]
        if body is not None:
            argv += ["-d", "@-"]
        result = subprocess.run(argv, input=None if body is None else json.dumps(body),
                                text=True, capture_output=True, env=self.env, timeout=60)
        codes = re.findall(r"HTTP/\S+\s+(\d+)", result.stderr)
        if missing_ok and not result.returncode and codes and codes[-1] == "404":
            return None
        # tea can exit zero for HTTP failures. Never interpret those as success.
        if result.returncode or not codes or not codes[-1].startswith("2"):
            raise RuntimeError(f"tea {method} {path}: HTTP {codes}, exit {result.returncode}")
        value = json.loads(result.stdout) if result.stdout.strip() else None
        return (value, result.stderr) if include_headers else value

    def remote(self, suffix, method="GET", body=None, *, missing_ok=False):
        return self.tea(f"repos/{self.args.repo}/{suffix}", method, body, missing_ok=missing_ok)

    def remote_list(self, suffix):
        items = []
        page = 1
        separator = "&" if "?" in suffix else "?"
        while True:
            batch, headers = self.tea(
                f"repos/{self.args.repo}/{suffix}{separator}limit=50&page={page}", include_headers=True)
            if not isinstance(batch, list):
                raise RuntimeError(f"expected a paginated array from {suffix}")
            items.extend(batch)
            # Some Forgejo list endpoints ignore page/limit and return the
            # entire collection. Follow the same Link contract as the client.
            total_pages = re.findall(r"^x-total-pages:\s*(\d+)\s*$", headers, re.I | re.M)
            has_next = (page < int(total_pages[-1]) if total_pages else
                        any(re.search(r'rel\s*=\s*"?next\b', line, re.I)
                            for line in headers.splitlines() if line.lower().startswith("link:")))
            if not has_next:
                return items
            page += 1

    def api(self, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.cfg['server']['port']}/api/v1/{path}",
            data=data, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=30) as response:
            result = json.load(response)
        if not result.get("ok"):
            raise RuntimeError(f"daemon API {path} failed; inspect artifacts")
        return result.get("data")

    def git(self, *args):
        return execute(["git", *args], cwd=self.checkout, env=self.env).strip()

    def setup(self):
        for name in ["go", "git", "tea", "codex", "python3", "ps"]:
            if not shutil.which(name):
                raise RuntimeError(f"required executable missing: {name}")
        execute(["go", "build", "-o", str(self.bin_dir) + "/",
                 "./cmd/looper", "./cmd/looperd"], cwd=ROOT, env=self.env)
        metadata = self.tea(f"repos/{self.args.repo}")
        expected_url = self.args.base_url.rstrip("/") + "/" + self.args.repo
        if metadata.get("html_url", "").rstrip("/") != expected_url:
            raise RuntimeError("tea login repository URL does not match --base-url and --repo")
        self.default_branch = metadata["default_branch"]
        self.initial_default_sha = self.remote("branches/" + urllib.parse.quote(self.default_branch, safe=""))["commit"]["id"]
        self.evidence["default_branch"] = self.default_branch
        self.evidence["initial_default_sha"] = self.initial_default_sha
        execute(["git", "clone", metadata["ssh_url"], self.checkout], env=self.env)
        self.git("config", "user.name", "Looper sandbox test")
        self.git("config", "user.email", "looper-sandbox@example.com")
        self.git("config", "commit.gpgsign", "false")
        self.git("checkout", "-b", self.branch, self.initial_default_sha)
        self.fixture = self.checkout / ("looper_smoke_" + self.run_id.replace("-", "_"))
        self.fixture.mkdir()
        (self.fixture / "price.py").write_text(
            'def discounted_price(price, discount_percent):\n'
            '    """Return the final price after applying a percentage discount."""\n'
            '    return price * discount_percent / 100\n')
        (self.fixture / "test_price.py").write_text(
            'import unittest\nfrom price import discounted_price\n\n'
            'class PriceTests(unittest.TestCase):\n'
            '    def test_twenty_percent(self):\n'
            '        self.assertEqual(discounted_price(100, 20), 80)\n'
            '    def test_no_discount(self):\n'
            '        self.assertEqual(discounted_price(100, 0), 100)\n'
            '    def test_full_discount(self):\n'
            '        self.assertEqual(discounted_price(100, 100), 0)\n')
        # Keep the acceptance contract outside the agent's editable checkout.
        (self.out / "test_price_contract.py").write_text((self.fixture / "test_price.py").read_text())
        (self.fixture / ".gitignore").write_text("__pycache__/\n")
        self.git("add", self.fixture.name)
        self.git("commit", "-m", "test: add Forgejo review and repair fixture")
        self.configure()
        save(self.out / "config.json", self.cfg)
        execute([self.bin_dir / "looper", "config", "validate", "--config", self.out / "config.json"], env=self.env)
        save(self.out / "resources.json", self.evidence)
        # Also attempt cleanup if the server accepted a push whose response was lost.
        self.pushed = True
        self.git("push", "origin", "HEAD:refs/heads/" + self.branch)
        self.label_attempted = True
        label = self.remote("labels", "POST", {"name": self.label_name, "color": "5319e7"})
        self.label_id = label["id"]
        self.evidence["label_id"] = self.label_id
        save(self.out / "resources.json", self.evidence)
        self.pr_attempted = True
        pr = self.remote("pulls", "POST", {
            "base": self.default_branch, "head": self.branch,
            "title": "test: Forgejo native review and repair " + self.run_id,
            "body": "Authorized Looper sandbox lifecycle test. Implement the documented final-price "
                    "function in the new fixture directory. Its unittest contract must pass. "
                    "Scope is only this fixture. Do not merge this PR. "
                    "Looper will close it after testing native review, repair and follow-up review.",
        })
        self.pr_number = pr["number"]
        self.evidence.update(pr_url=pr["html_url"], pr_number=self.pr_number,
                             original_head=pr["head"]["sha"], branch=self.branch, label_id=self.label_id)
        self.remote(f"issues/{self.pr_number}/labels", "POST", {"labels": [self.label_id]})
        save(self.out / "resources.json", self.evidence)

    def configure(self):
        # A minimal file lets looperd merge its own defaults. `config show`
        # reads a running daemon, so it must never bootstrap this isolated test.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        self.cfg = {
            "server": {"host": "127.0.0.1", "port": port, "authMode": "none"},
            "storage": {"dbPath": str(self.out / "looper.sqlite"), "backupDir": str(self.out / "backups")},
            "daemon": {"logDir": str(self.out / "logs"), "workingDirectory": str(self.checkout),
                       "mode": "foreground", "shutdownTimeoutMs": 30000, "worktreeCleanup": {"enabled": False}},
            "package": {"autoUpgradeEnabled": False},
            "notifications": {"osascript": {"enabled": False}, "webhook": {"enabled": False}},
            "webhook": {"enabled": False},
            "instructions": {"enabled": False},
            "disclosure": {"enabled": True, "includeAgent": True, "includeOS": False},
            "providers": [{"id": "forgejo-smoke", "kind": "forgejo", "baseUrl": self.args.base_url,
                           "auth": "tea", "teaLogin": self.args.tea_login}],
            "projects": [{"id": "forgejo-smoke", "name": "Isolated Forgejo smoke", "repo": self.args.repo,
                          "repoPath": str(self.checkout), "provider": "forgejo-smoke",
                          "worktreeRoot": str(self.out / "worktrees")}],
            "tools": {"looperPath": str(self.bin_dir / "looper")},
            "scheduler": {"pollIntervalSeconds": 10, "discoveryCacheTtlSeconds": 1},
            "roles": {
                "planner": {"autoDiscovery": False}, "worker": {"autoDiscovery": False},
                "coordinator": {"enabled": False},
                "reviewer": {
                    "autoMerge": {"enabled": False},
                    "discovery": {"autoDiscovery": False, "triggers": {
                        "enableSelfReview": True, "requireReviewRequest": False, "labels": [self.label_name]}},
                    "behavior": {"publishMode": "single_review", "loop": {
                        "enabledByDefault": True, "quietPeriodSeconds": 0, "minPublishIntervalSeconds": 0}}},
                "fixer": {"autoDiscovery": False, "triggers": {"labels": [self.label_name]},
                          "behavior": {"loop": {"quietPeriodSeconds": 0}}}},
            "defaults": {"allowAutoCommit": True, "allowAutoPush": True, "loop": {"quietPeriodSeconds": 0}},
            "agent": {"vendor": "codex", "nativeResume": {"enabled": False}, "timeouts": {
                "reviewerMaxRuntimeSeconds": 600, "reviewerIdleTimeoutSeconds": 300,
                "fixerMaxRuntimeSeconds": 600, "fixerIdleTimeoutSeconds": 300}},
        }

    def start(self):
        save(self.out / "config.json", self.cfg)
        self.log = (self.out / "daemon.log").open("a")
        self.proc = subprocess.Popen([str(self.bin_dir / "looperd"), "--config", str(self.out / "config.json")],
                                     cwd=self.checkout, env=self.env, stdout=self.log, stderr=self.log,
                                     start_new_session=True)
        self.session_id = self.proc.pid
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                raise RuntimeError("isolated daemon exited; inspect daemon.log")
            try:
                status = self.api("healthz")
                if Path(status["storage"]["dbPath"]).resolve() != self.out / "looper.sqlite":
                    raise RuntimeError("readiness reached a different daemon; refusing to use it")
                return
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.5)
        raise RuntimeError("isolated daemon did not become ready")

    def stop(self):
        if self.proc is not None:
            # Agents have their own process groups inside this dedicated
            # session. Killing only the daemon group would leave them alive.
            self.session_processes()
            if self.proc.poll() is None:
                self.proc.terminate()
            for sig, seconds in [(None, 35), (signal.SIGTERM, 5), (signal.SIGKILL, 5)]:
                members = self.session_processes()
                if sig is not None:
                    for pgid in set(members.values()):
                        # Recheck ownership immediately before signalling.
                        if pgid in self.session_processes().values():
                            try:
                                os.killpg(pgid, sig)
                            except ProcessLookupError:
                                pass
                deadline = time.monotonic() + seconds
                while members and time.monotonic() < deadline:
                    self.proc.poll()
                    time.sleep(0.2)
                    members = self.session_processes()
                if not members:
                    self.proc.wait(timeout=2)
                    break
            else:
                raise RuntimeError(f"own daemon session {self.session_id} still has live processes; remote cleanup withheld")
        self.proc = None
        self.session_id = None
        if self.log:
            self.log.close()
            self.log = None

    def session_processes(self):
        """Inspect IDs only; never match or signal other Looper/Codex by name."""
        rows = {}
        for line in execute(["ps", "-axo", "pid=,ppid=,pgid=,stat="], env=self.env, timeout=10).splitlines():
            pid, parent, group, state = line.split()
            rows[int(pid)] = (int(parent), int(group), state)
        descendants = {self.proc.pid}
        while True:
            expanded = descendants | {pid for pid, row in rows.items() if row[0] in descendants}
            if expanded == descendants:
                break
            descendants = expanded
        members = {}
        for pid, (_, group, state) in rows.items():
            if state.startswith("Z"):
                continue
            try:
                session = os.getsid(pid)
            except ProcessLookupError:
                continue
            if pid in descendants and session != self.session_id:
                raise RuntimeError(f"own child {pid} escaped daemon session; cannot confirm cleanup, remote deletion withheld")
            if session == self.session_id:
                members[pid] = group
        return members

    def database_state(self):
        # Read the already isolated DB, including completed agent executions;
        # successful skipped runs alone cannot establish that no agent reran.
        with sqlite3.connect((self.out / "looper.sqlite").as_uri() + "?mode=ro", uri=True) as db:
            db.row_factory = sqlite3.Row
            return {
                "executions": [dict(row) for row in db.execute(
                    "SELECT id, run_id, vendor, status FROM agent_executions ORDER BY id")],
                "queue": [dict(row) for row in db.execute(
                    "SELECT id, loop_id, status FROM queue_items ORDER BY id")],
            }

    def wait_runs(self, predicate, phase):
        deadline = time.monotonic() + self.args.timeout
        last = None
        while time.monotonic() < deadline:
            runs = self.api("runs")["items"]
            loops = self.api("loops")
            save(self.out / (phase + "-runs.json"), runs)
            save(self.out / (phase + "-loops.json"), loops)
            state = [(r["id"], r["status"]) for r in runs]
            if state != last:
                print(phase, state, flush=True)
                last = state
            if any(r["status"] in {"failed", "cancelled", "interrupted", "parse_failed"} for r in runs):
                raise RuntimeError(f"{phase}: run failed; inspect saved runs/daemon log")
            if predicate(runs):
                return runs
            time.sleep(3)
        raise RuntimeError(f"{phase} timed out; inspect artifacts")

    def snapshot(self):
        pr = self.remote(f"pulls/{self.pr_number}")
        reviews = self.remote_list(f"pulls/{self.pr_number}/reviews")
        comments = self.remote_list(f"issues/{self.pr_number}/comments")
        return {"pr": pr, "reviews": reviews, "comments": comments}

    @staticmethod
    def message_state(snapshot):
        return {kind: sorted((entry["id"], entry["body"]) for entry in snapshot[kind])
                for kind in ["reviews", "comments"]}

    def wait_restart_stable(self, before, before_runs, before_db):
        before_ids = {run["id"] for run in before_runs}
        stable_window = 2 * self.cfg["scheduler"]["pollIntervalSeconds"] + 2
        stable_since = None
        last_state = None
        deadline = time.monotonic() + self.args.timeout
        while time.monotonic() < deadline:
            runs = self.api("runs")["items"]
            database = self.database_state()
            after = self.snapshot()
            save(self.out / "restart-runs.json", runs)
            save(self.out / "restart-database.json", database)
            save(self.out / "restart-snapshot.json", after)
            if (before["pr"]["head"]["sha"] != after["pr"]["head"]["sha"]
                    or self.message_state(before) != self.message_state(after)):
                raise RuntimeError("restart changed head or review/fixer messages")
            if database["executions"] != before_db["executions"]:
                raise RuntimeError("restart started another agent execution")
            if not before_ids.issubset({run["id"] for run in runs}):
                raise RuntimeError("restart lost prior runs")
            if any(run["status"] in {"failed", "cancelled", "interrupted", "parse_failed"} for run in runs):
                raise RuntimeError("restart produced a terminal run failure")
            extra = [run for run in runs if run["id"] not in before_ids]
            for run in extra:
                if run["status"] == "success" and not json.loads(run.get("checkpointJson") or "{}").get("skipReason"):
                    raise RuntimeError("restart produced new work without an explicit skipped checkpoint")
            quiet = (all(run["status"] == "success" for run in runs)
                     and all(row["status"] in {"completed", "cancelled"} for row in database["queue"]))
            state = ([run["id"] for run in runs], database["queue"])
            if not quiet or state != last_state:
                stable_since = time.monotonic() if quiet else None
            elif stable_since is not None and time.monotonic() - stable_since >= stable_window:
                self.evidence["restart_skipped_only_runs"] = [run["id"] for run in extra]
                if extra:
                    print("restart allowed skipped-only runs (no agent execution):", len(extra), flush=True)
                return runs, after
            last_state = state
            time.sleep(3)
        raise RuntimeError("restart did not reach a stable empty queue; inspect artifacts")

    def run(self):
        self.setup()
        print("PR", self.evidence["pr_url"], "artifacts", self.out, flush=True)
        self.start()
        # No force bypass: exercises the actual provider-aware manual entrypoint.
        loop = self.api("loops", {"projectId": "forgejo-smoke", "type": "reviewer",
                                 "targetType": "pull_request", "repo": self.args.repo,
                                 "prNumber": self.pr_number, "metadata": {"manual": True}})
        self.wait_runs(lambda runs: any(r["loopId"] == loop["id"] and r["status"] == "success"
                                       for r in runs), "manual-review")
        first = self.snapshot()
        actionable = [r for r in first["reviews"] if re.search(r"outcome=(blocking|non_blocking)", r["body"])]
        if not actionable:
            raise RuntimeError("reviewer did not publish an actionable native review for the known bug")
        native_comments = []
        for review in actionable:
            # Forgejo returns the complete review's comments here; this endpoint
            # has no page/limit parameters and repeats its result if given them.
            native_comments += self.remote(f"pulls/{self.pr_number}/reviews/{review['id']}/comments")
        if not native_comments:
            raise RuntimeError("actionable native review has no inline findings")
        save(self.out / "initial-native.json", {"snapshot": first, "inline_comments": native_comments})
        self.stop()
        self.cfg["roles"]["reviewer"]["discovery"]["autoDiscovery"] = True
        self.cfg["roles"]["fixer"]["autoDiscovery"] = True
        self.start()

        def converged(runs):
            if not runs or any(r["status"] not in {"success"} for r in runs):
                return False
            snap = self.snapshot()
            head = snap["pr"]["head"]["sha"]
            if head == self.evidence["original_head"]:
                return False
            clean = [r for r in snap["reviews"] if "outcome=clean" in r["body"] and r.get("commit_id") == head]
            if clean and all(row["status"] in {"completed", "cancelled"} for row in self.database_state()["queue"]):
                save(self.out / "converged.json", snap)
                return True
            return False

        before_runs = self.wait_runs(converged, "automatic-repair-and-review")
        before = self.snapshot()
        before_db = self.database_state()
        save(self.out / "before-restart-database.json", before_db)
        fixer_loops = {loop["id"] for loop in self.api("loops")["items"]
                       if loop["type"] == "fixer" and not json.loads(loop.get("metadataJson") or "{}").get("manual")}
        fixed_ids = set()
        for run in before_runs:
            if run["loopId"] not in fixer_loops:
                continue
            checkpoint = json.loads(run.get("checkpointJson") or "{}")
            if not checkpoint.get("validation", {}).get("passed") or not checkpoint.get("push", {}).get("pushed"):
                continue
            native = {item["id"]: item["providerCommentId"] for item in checkpoint.get("fixItems", [])
                      if item.get("source") == "forgejo_review_comment"}
            fixed_ids.update(native[item["fixItemId"]] for item in checkpoint.get("resolvedComments", {}).get("items", [])
                             if item.get("status") == "fixed_unresolved" and item.get("fixItemId") in native)
        if not {comment["id"] for comment in native_comments}.issubset(fixed_ids):
            raise RuntimeError("missing successful automatic native fixer validation/push/acknowledgement checkpoint")
        self.stop()
        self.start()
        runs, after = self.wait_restart_stable(before, before_runs, before_db)
        self.stop()
        fixer_comments = [c for c in after["comments"] if "runner=fixer" in c["body"]]
        if not fixer_comments:
            raise RuntimeError("no common fixer acknowledgement/disclosure was published")
        remaining = []
        for review in actionable:
            remaining += self.remote(f"pulls/{self.pr_number}/reviews/{review['id']}/comments")
        by_id = {c["id"]: c for c in remaining}
        if any(c["id"] not in by_id or by_id[c["id"]].get("resolver") is not None
               for c in native_comments):
            raise RuntimeError("native findings were deleted or remotely resolved during the no-resolve test")
        save(self.out / "final-native-comments.json", remaining)
        if not any("code fixed; comment remains open" in c["body"] for c in fixer_comments):
            raise RuntimeError("fixer did not describe the code fix and still-open comment accurately")
        for body in [r["body"] for r in after["reviews"]] + [c["body"] for c in fixer_comments]:
            visible = re.sub(r"<!--.*?-->", "", body, flags=re.S)
            if "Reviewer Summary" in visible or "Looper Forgejo Fixer Summary" in visible:
                raise RuntimeError("legacy protocol title leaked into visible message")
            if ("🔁 Powered by " not in body or "agent=codex" not in body
                    or "An autonomous AI dev team for your GitHub repos." not in body):
                raise RuntimeError("message does not use the shared Codex disclosure")
        final_head = after["pr"]["head"]["sha"]
        self.git("fetch", "origin", final_head)
        self.git("checkout", "--detach", final_head)
        result = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", self.fixture.name, "-v"],
                                cwd=self.checkout, env=self.env, text=True, capture_output=True, timeout=60)
        (self.out / "fixture-test.log").write_text(result.stdout + result.stderr)
        if result.returncode:
            raise RuntimeError("pushed fixture tests fail; inspect fixture-test.log")
        contract_env = dict(self.env, PYTHONPATH=str(self.fixture))
        contract = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", str(self.out),
                                   "-p", "test_price_contract.py", "-v"],
                                  cwd=self.out, env=contract_env, text=True, capture_output=True, timeout=60)
        (self.out / "fixture-contract.log").write_text(contract.stdout + contract.stderr)
        if contract.returncode:
            raise RuntimeError("pushed code fails the original acceptance contract; inspect fixture-contract.log")
        self.evidence.update(final_head=final_head, native_reviews=len(after["reviews"]),
                             fixer_comments=len(fixer_comments), open_native_findings=len(native_comments),
                             restart_stable=True, fixture_tests="passed")
        save(self.out / "result.json", self.evidence)

    def cleanup(self):
        errors = []
        try:
            self.stop()
        except Exception as error:
            save(self.out / "cleanup.json", {"errors": [str(error)], "remote_cleanup_withheld": True,
                                             "resources": self.evidence})
            raise RuntimeError("cannot confirm own processes stopped; remote cleanup withheld, see cleanup.json") from error
        # A successful POST may lose its response. Find only this run's exact
        # unique name/head before deciding there is nothing left to clean.
        try:
            if self.label_attempted and self.label_id is None:
                matches = [label for label in self.remote_list("labels") if label["name"] == self.label_name]
                if len(matches) > 1:
                    raise RuntimeError("multiple labels match this run; refusing ambiguous cleanup")
                if matches:
                    self.label_id = matches[0]["id"]
                    self.evidence["label_id"] = self.label_id
            if self.pr_attempted and self.pr_number is None:
                matches = [pr for pr in self.remote_list("pulls?state=all")
                           if pr["head"]["ref"] == self.branch
                           and pr["head"].get("repo", {}).get("full_name") == self.args.repo]
                if len(matches) > 1:
                    raise RuntimeError("multiple PRs match this run; refusing ambiguous cleanup")
                if matches:
                    self.pr_number = matches[0]["number"]
                    self.evidence.update(pr_number=self.pr_number, pr_url=matches[0]["html_url"])
        except Exception as error:
            save(self.out / "cleanup.json", {"errors": [str(error)], "remote_cleanup_withheld": True,
                                             "resources": self.evidence})
            raise RuntimeError("could not recover this run's resource IDs; cleanup withheld, see cleanup.json") from error
        actions = []
        if self.pr_number:
            actions.append((f"pulls/{self.pr_number}", "PATCH", {"state": "closed"}))
        if self.pushed:
            actions.append((f"branches/{self.branch}", "DELETE", None))
        if self.label_id:
            actions.append((f"labels/{self.label_id}", "DELETE", None))
        for path, method, body in actions:
            try:
                self.remote(path, method, body, missing_ok=True)
            except Exception as error:
                errors.append(str(error))
        if self.default_branch and self.initial_default_sha:
            try:
                current = self.remote("branches/" + urllib.parse.quote(self.default_branch, safe=""))["commit"]["id"]
                if current != self.initial_default_sha:
                    errors.append("default branch changed during test; inspect external activity")
            except Exception as error:
                errors.append(str(error))
        for path in ([f"branches/{self.branch}"] if self.pushed else []) + ([f"labels/{self.label_id}"] if self.label_id else []):
            try:
                if self.remote(path, missing_ok=True) is not None:
                    errors.append("test resource remains: " + path)
            except Exception as error:
                errors.append(str(error))
        if self.pr_number:
            try:
                pr = self.remote(f"pulls/{self.pr_number}", missing_ok=True)
                if pr and (pr["state"] != "closed" or pr.get("merged")):
                    errors.append("test PR was not closed unmerged")
            except Exception as error:
                errors.append(str(error))
        save(self.out / "cleanup.json", {"errors": errors, "resources": self.evidence})
        if errors:
            raise RuntimeError("cleanup needs attention; see cleanup.json")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--repo", required=True, help="Explicit sandbox owner/repo (writes test PR/branch/label)")
    parser.add_argument("--tea-login", required=True)
    parser.add_argument("--output", help="New artifact directory; defaults to dist/looper-smoke-<run>")
    parser.add_argument("--timeout", type=int, default=1200, help="Maximum seconds per lifecycle phase")
    args = parser.parse_args()
    if not re.fullmatch(r"[^/\s]+/[^/\s]+", args.repo):
        parser.error("--repo must be owner/repo")
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    smoke = Smoke(args)
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    try:
        smoke.run()
    finally:
        smoke.cleanup()
    print("PASS", smoke.evidence["pr_url"], smoke.out, flush=True)


if __name__ == "__main__":
    main()
