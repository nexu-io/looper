import copy
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "review_thread_gate", Path(__file__).with_name("configure-review-thread-gate.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class ReviewThreadGateTest(unittest.TestCase):
    def test_preserves_existing_rules_and_review_requirements(self):
        existing = gate.gate_body(None, "main")
        existing["rules"][0]["parameters"]["required_approving_review_count"] = 2
        existing["rules"][0]["parameters"]["required_review_thread_resolution"] = False
        existing["rules"].append({"type": "deletion"})
        before = copy.deepcopy(existing)
        desired = gate.gate_body(existing, "main")
        self.assertEqual(existing, before)
        self.assertEqual(desired["rules"][0]["parameters"]["required_approving_review_count"], 2)
        self.assertTrue(desired["rules"][0]["parameters"]["required_review_thread_resolution"])
        self.assertEqual(desired["rules"][1], {"type": "deletion"})

    def test_refuses_unexpected_scope_or_bypass(self):
        existing = gate.gate_body(None, "other")
        with self.assertRaisesRegex(ValueError, "different scope"):
            gate.gate_body(existing, "main")
        existing = gate.gate_body(None, "main")
        existing["bypass_actors"] = [{"actor_type": "RepositoryRole", "actor_id": 5}]
        with self.assertRaisesRegex(ValueError, "bypass"):
            gate.gate_body(existing, "main")

    def run_workflow(self, latest="abc", readback_valid=True, existing=False):
        calls = []
        body = gate.gate_body(None, "main")

        def api(method, path, token, payload=None):
            calls.append((method, path, token, payload))
            if method in ("PUT", "POST"):
                self.assertEqual(calls[-2][1], "repos/nexu-io/looper/git/ref/heads/main")
                return {"id": 42}
            if path.endswith("git/ref/heads/main"):
                return {"object": {"sha": latest}}
            if "rulesets?" in path:
                return [{"id": 42, "name": body["name"]}] if existing else []
            if path.endswith("rulesets/42"):
                result = copy.deepcopy(body)
                result["rules"][0]["parameters"]["allowed_merge_methods"] = ["squash", "merge"]
                if not readback_valid and any(call[0] in ("POST", "PUT") for call in calls):
                    result["rules"][0]["parameters"]["required_review_thread_resolution"] = False
                return result
            return {}

        with tempfile.TemporaryDirectory() as directory:
            env = {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main",
                   "GITHUB_REPOSITORY": "nexu-io/looper", "GITHUB_SHA": "abc",
                   "GITHUB_RUN_ID": "123", "GITHUB_TOKEN": "read-source",
                   "REVIEW_GATE_TOKEN": "write-target", "TARGET_REPOSITORY": "nexu-io/openmotion",
                   "TARGET_BRANCH": "main", "GITHUB_STEP_SUMMARY": str(Path(directory) / "summary")}
            with patch.dict(os.environ, env), patch.object(gate, "api", side_effect=api):
                try:
                    gate.main()
                finally:
                    self.calls = calls

    def test_creates_and_updates_only_its_own_ruleset(self):
        for existing, method in ((False, "POST"), (True, "PUT")):
            with self.subTest(existing=existing):
                self.run_workflow(existing=existing)
                writes = [call for call in self.calls if call[0] in ("POST", "PUT")]
                self.assertEqual(len(writes), 1)
                self.assertEqual(writes[0][0], method)
                self.assertEqual(writes[0][2], "write-target")

    def test_rejects_stale_run_before_any_write(self):
        with self.assertRaisesRegex(ValueError, "latest main"):
            self.run_workflow(latest="new-main")
        self.assertFalse(any(call[0] in ("POST", "PUT") for call in self.calls))

    def test_detects_failed_rule_readback(self):
        with self.assertRaisesRegex(ValueError, "readback"):
            self.run_workflow(readback_valid=False)

    def test_rejects_non_main_or_local_execution(self):
        for values in ({"GITHUB_ACTIONS": "false", "GITHUB_REF": "refs/heads/main"},
                       {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/feature"}):
            with self.subTest(values=values), patch.dict(os.environ, {
                    "GITHUB_REPOSITORY": "nexu-io/looper", "GITHUB_SHA": "abc",
                    "TARGET_REPOSITORY": "nexu-io/openmotion", "TARGET_BRANCH": "main", **values}):
                with self.assertRaisesRegex(ValueError, "main-branch GitHub workflow"):
                    gate.main()


if __name__ == "__main__":
    unittest.main()
