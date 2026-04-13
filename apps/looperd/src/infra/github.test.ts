import { afterEach, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { GhCliGitHubGateway, ReviewThreadNotFoundError } from "./github";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

describe("GhCliGitHubGateway", () => {
  test("lists, snapshots, and reviews pull requests through gh", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-gh-"));
    cleanupPaths.push(rootDir);

    const logPath = join(rootDir, "gh.log");
    const scriptPath = join(rootDir, "gh");
    await writeFile(
      scriptPath,
      `#!/bin/sh\nprintf '%s\n' "$*" >> "${logPath}"\ncase "$*" in
  "pr list"*)
    printf '[{"number":42,"title":"Review me","url":"https://example.test/pr/42","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","author":{"login":"octocat"},"reviewRequests":[{"__typename":"User","login":"OctoCat"},{"__typename":"Team","slug":"platform"}]}]'
    ;;
  "label create"*)
    printf '{}'
    ;;
  "issue list"*)
    printf '[{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}]'
    ;;
  "issue view"*)
    printf '{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}'
    ;;
  "api repos/acme/looper/issues/42/labels --method POST -f labels[]=phase-1 -f labels[]=ready")
    printf '{}'
    ;;
  "api repos/acme/looper/issues/42/labels/needs-work --method DELETE")
    printf '{}'
    ;;
  "api repos/acme/looper/pulls/42/requested_reviewers --method POST -f reviewers[]=reviewer")
    printf '{}'
    ;;
  "pr view"*)
    printf '{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pr/42","state":"OPEN","isDraft":false,"reviewDecision":"CHANGES_REQUESTED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","author":{"login":"octocat"},"reviewRequests":[{"requestedReviewer":{"__typename":"User","login":"reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"platform"}}],"comments":[{"state":"UNRESOLVED"}],"reviews":[{"state":"COMMENTED"}],"statusCheckRollup":[{"conclusion":"SUCCESS"}]}'
    ;;
  "pr diff"*)
    printf 'diff --git a/a.ts b/a.ts\n'
    ;;
  "api user"*)
    printf 'reviewer\n'
    ;;
  *"resolveReviewThread"*)
    printf '{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}'
    ;;
  *"reviewThreads"*)
    printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"id":"comment-1","body":"Fix this"}]}}]}}}}}'
    ;;
  *"threadId=thread-1"*)
    printf '{"data":{"node":{"id":"thread-1","isResolved":false}}}'
    ;;
esac\n`,
    );
    await chmod(scriptPath, 0o755);

    const gateway = new GhCliGitHubGateway({
      ghPath: scriptPath,
      cwd: rootDir,
    });
    const prs = await gateway.listOpenPullRequests({
      repo: "acme/looper",
      label: "phase-1",
    });
    const issues = await gateway.listOpenIssues({
      repo: "acme/looper",
      assignee: "reviewer",
      label: "phase-1",
    });
    const issueDetail = await gateway.viewIssue({
      repo: "acme/looper",
      issueNumber: 8,
    });
    const snapshot = await gateway.capturePullRequestSnapshot({
      projectId: "project_1",
      repo: "acme/looper",
      prNumber: 42,
    });
    await gateway.submitReview({
      repo: "acme/looper",
      prNumber: 42,
      event: "COMMENT",
      body: "Looks good",
    });
    await gateway.resolveReviewThread({
      repo: "acme/looper",
      threadId: "thread-1",
    });
    await gateway.addPullRequestLabels({
      repo: "acme/looper",
      prNumber: 42,
      labels: ["phase-1", "ready"],
    });
    await gateway.removePullRequestLabels({
      repo: "acme/looper",
      prNumber: 42,
      labels: ["needs-work"],
    });
    await gateway.addPullRequestReviewers({
      repo: "acme/looper",
      prNumber: 42,
      reviewers: ["reviewer"],
    });

    expect(prs[0]?.number).toBe(42);
    expect(prs[0]?.reviewRequests).toEqual(["OctoCat"]);
    expect(issues[0]?.assignees).toEqual(["reviewer"]);
    expect(issues[0]?.labels).toEqual(["phase-1", "gateway"]);
    expect(issueDetail.number).toBe(8);
    expect(snapshot.headSha).toBe("abc123");
    expect(snapshot.reviewState).toBe("CHANGES_REQUESTED");
    const detail = await gateway.viewPullRequest({
      repo: "acme/looper",
      prNumber: 42,
    });
    expect(detail.reviewRequests).toEqual(["reviewer"]);
    expect(detail.comments).toEqual([
      {
        id: "comment-1",
        threadId: "thread-1",
        state: "UNRESOLVED",
        isResolved: false,
        body: "Fix this",
      },
    ]);
    await expect(gateway.getCurrentUserLogin()).resolves.toBe("reviewer");

    const log = await readFile(logPath, "utf8");
    expect(log).toContain(
      "pr review 42 --repo acme/looper --comment --body Looks good",
    );
    expect(log).toContain(
      "pr list --repo acme/looper --state open --limit 30 --label phase-1",
    );
    expect(log).toContain(
      "issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label phase-1",
    );
    expect(log).toContain("issue view 8 --repo acme/looper");
    expect(log).toContain(
      "label create phase-1 --repo acme/looper --color 5319e7 --description Managed by looper --force",
    );
    expect(log).toContain(
      "label create ready --repo acme/looper --color 5319e7 --description Managed by looper --force",
    );
    expect(log).toContain(
      "api repos/acme/looper/issues/42/labels --method POST -f labels[]=phase-1 -f labels[]=ready",
    );
    expect(log).toContain(
      "api repos/acme/looper/issues/42/labels/needs-work --method DELETE",
    );
    expect(log).toContain(
      "api repos/acme/looper/pulls/42/requested_reviewers --method POST -f reviewers[]=reviewer",
    );
    expect(log).toContain("threadId=thread-1");
  });

  test("treats missing review thread as not found", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-gh-"));
    cleanupPaths.push(rootDir);

    const scriptPath = join(rootDir, "gh");
    await writeFile(
      scriptPath,
      `#!/bin/sh
case "$*" in
  *"threadId=thread-missing"*)
    printf '{"data":{"node":null}}'
    ;;
  *)
    printf '{}'
    ;;
esac
`,
    );
    await chmod(scriptPath, 0o755);

    const gateway = new GhCliGitHubGateway({
      ghPath: scriptPath,
      cwd: rootDir,
    });

    await expect(
      gateway.resolveReviewThread({
        repo: "acme/looper",
        threadId: "thread-missing",
      }),
    ).rejects.toBeInstanceOf(ReviewThreadNotFoundError);
  });

  test("surfaces permission errors when resolving review thread", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-gh-"));
    cleanupPaths.push(rootDir);

    const scriptPath = join(rootDir, "gh");
    await writeFile(
      scriptPath,
      `#!/bin/sh
case "$*" in
  *"resolveReviewThread"*)
    printf 'permission denied' >&2
    exit 1
    ;;
  *"threadId=thread-1"*)
    printf '{"data":{"node":{"id":"thread-1","isResolved":false}}}'
    ;;
  *)
    printf '{}'
    ;;
esac
`,
    );
    await chmod(scriptPath, 0o755);

    const gateway = new GhCliGitHubGateway({
      ghPath: scriptPath,
      cwd: rootDir,
    });

    await expect(
      gateway.resolveReviewThread({
        repo: "acme/looper",
        threadId: "thread-1",
      }),
    ).rejects.toThrow("Command exited with code 1");
  });

  test("ignores missing label errors when removing pull request labels", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-gh-"));
    cleanupPaths.push(rootDir);

    const scriptPath = join(rootDir, "gh");
    await writeFile(
      scriptPath,
      `#!/bin/sh
case "$*" in
  "api repos/acme/looper/issues/42/labels/looper%3Aspec-ready --method DELETE")
    printf 'gh: HTTP 404: label does not exist (https://api.github.com/...)' >&2
    exit 1
    ;;
  *)
    printf '{}'
    ;;
esac
`,
    );
    await chmod(scriptPath, 0o755);

    const gateway = new GhCliGitHubGateway({
      ghPath: scriptPath,
      cwd: rootDir,
    });

    await expect(
      gateway.removePullRequestLabels({
        repo: "acme/looper",
        prNumber: 42,
        labels: ["looper:spec-ready"],
      }),
    ).resolves.toBeUndefined();
  });
});
