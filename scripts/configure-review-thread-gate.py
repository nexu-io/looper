#!/usr/bin/env python3
"""Configure GitHub's native conversation-resolution rule from trusted CI."""

import copy
import json
import os
import re
import urllib.parse
import urllib.request

RULESET_NAME = "Looper review conversations"


def api(method, path, token, body=None):
    request = urllib.request.Request(
        "https://api.github.com/" + path,
        data=None if body is None else json.dumps(body).encode(),
        method=method,
        headers={
            "Authorization": "Bearer " + token,
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def gate_body(existing, branch):
    conditions = {"ref_name": {"include": ["refs/heads/" + branch], "exclude": []}}
    if existing:
        if (existing.get("target") != "branch" or existing.get("conditions") != conditions
                or existing.get("bypass_actors")):
            raise ValueError("Existing gate has different scope or bypass actors; review it manually")
        rules = copy.deepcopy(existing["rules"])
    else:
        rules = []
    pull_request = next((rule for rule in rules if rule["type"] == "pull_request"), None)
    if pull_request is None:
        pull_request = {"type": "pull_request", "parameters": {
            "required_approving_review_count": 0,
            "dismiss_stale_reviews_on_push": False,
            "require_code_owner_review": False,
            "require_last_push_approval": False,
            "required_review_thread_resolution": True,
        }}
        rules.append(pull_request)
    pull_request["parameters"]["required_review_thread_resolution"] = True
    return {"name": f"{RULESET_NAME} ({branch})", "target": "branch", "enforcement": "active",
            "bypass_actors": [], "conditions": conditions, "rules": rules}


def main():
    source = os.environ["GITHUB_REPOSITORY"]
    sha = os.environ["GITHUB_SHA"]
    target = os.environ["TARGET_REPOSITORY"]
    branch = os.environ["TARGET_BRANCH"]
    if (os.environ.get("GITHUB_ACTIONS") != "true"
            or os.environ.get("GITHUB_REF") != "refs/heads/main"):
        raise ValueError("Run this operation through the main-branch GitHub workflow")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", target):
        raise ValueError("Target repository must use owner/repo format")
    if not re.fullmatch(r"[A-Za-z0-9_./-]+", branch):
        raise ValueError("Invalid target branch")
    read_token = os.environ["GITHUB_TOKEN"]
    write_token = os.environ["REVIEW_GATE_TOKEN"]
    api("GET", f"repos/{target}/branches/{urllib.parse.quote(branch, safe='')}", write_token)
    matches = []
    page = 1
    while True:
        batch = api("GET", f"repos/{target}/rulesets?includes_parents=false&per_page=100&page={page}", write_token)
        matches.extend(rule for rule in batch if rule["name"] == f"{RULESET_NAME} ({branch})")
        if len(batch) < 100:
            break
        page += 1
    if len(matches) > 1:
        raise ValueError("Multiple gate rulesets found; review them manually")
    path = f"repos/{target}/rulesets"
    existing = None
    if matches:
        path += "/" + str(matches[0]["id"])
        existing = api("GET", path, write_token)
    body = gate_body(existing, branch)
    latest = api("GET", f"repos/{source}/git/ref/heads/main", read_token)["object"]["sha"]
    if latest != sha:
        raise ValueError("Workflow SHA is no longer latest main; start a new run")
    # Keep the latest-main check immediately before the only production write.
    result = api("PUT" if existing else "POST", path, write_token, body)
    verified = api("GET", f"repos/{target}/rulesets/{result['id']}", write_token)
    if any(verified.get(key) != value for key, value in body.items() if key != "rules"):
        raise ValueError("Gate readback did not match the requested rule")
    for expected in body["rules"]:
        actual = next((rule for rule in verified["rules"] if rule["type"] == expected["type"]), None)
        if actual is None or any(
                actual.get("parameters", {}).get(key) != value
                for key, value in expected.get("parameters", {}).items()):
            raise ValueError("Gate readback did not match the requested rule parameters")
    summary = (f"Enabled native review conversation resolution for `{target}` → `{branch}`.\n\n"
               f"Ruleset ID `{result['id']}`. Source SHA `{sha}`. Run `{os.environ['GITHUB_RUN_ID']}`.\n\n"
               "Every unresolved native review thread blocks merging, including human reviews. "
               "Top-level PR comments are outside this rule. Resolving a thread clears that "
               "thread's gate without proving that the code was repaired.\n")
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as output:
        output.write(summary)


if __name__ == "__main__":
    main()
