import { randomUUID } from "node:crypto";

import type { PullRequestSnapshotRecord } from "../storage/types";
import { CommandExecutionError, runCommand } from "./command";

export interface GitHubGatewayOptions {
  ghPath: string;
  cwd?: string;
  now?: () => Date;
}

export interface GitHubPullRequestSummary {
  number: number;
  title: string;
  url?: string;
  state?: string;
  isDraft: boolean;
  reviewDecision?: string;
  labels: string[];
  headRefName?: string;
  baseRefName?: string;
  headSha?: string;
  author?: string;
  reviewRequests: string[];
}

export interface GitHubPullRequestDetail extends GitHubPullRequestSummary {
  body?: string;
  headSha?: string;
  baseSha?: string;
  comments: unknown[];
  reviews: unknown[];
  checks: unknown[];
}

export interface GitHubIssueSummary {
  number: number;
  title: string;
  body?: string;
  url?: string;
  state?: string;
  author?: string;
  assignees: string[];
  labels: string[];
}

export interface GitHubIssueDetail extends GitHubIssueSummary {}

export interface SubmitReviewInput {
  repo: string;
  prNumber: number;
  event: "APPROVE" | "COMMENT" | "REQUEST_CHANGES";
  body?: string;
  cwd?: string;
}

export interface CreatePullRequestInput {
  repo: string;
  headBranch: string;
  baseBranch: string;
  title: string;
  body?: string;
  cwd?: string;
}

export interface CreatePullRequestResult {
  number?: number;
  url: string;
}

export class ReviewThreadNotFoundError extends Error {
  constructor(threadId: string) {
    super(`Review thread not found: ${threadId}`);
    this.name = "ReviewThreadNotFoundError";
  }
}

export class GhCliGitHubGateway {
  private readonly now: () => Date;

  constructor(private readonly options: GitHubGatewayOptions) {
    this.now = options.now ?? (() => new Date());
  }

  public async listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    label?: string;
  }): Promise<GitHubPullRequestSummary[]> {
    const args = [
      "pr",
      "list",
      "--repo",
      input.repo,
      "--state",
      "open",
      "--limit",
      String(input.limit ?? 30),
    ];
    if (input.label) {
      args.push("--label", input.label);
    }
    args.push(
      "--json",
      [
        "number",
        "title",
        "url",
        "state",
        "isDraft",
        "reviewDecision",
        "labels",
        "headRefName",
        "baseRefName",
        "headRefOid",
        "author",
        "reviewRequests",
      ].join(","),
    );

    const result = await this.runGh(args, input.cwd);

    return asArray(result.stdout).map((item) => ({
      number: Number(item.number),
      title: String(item.title),
      url: asOptionalString(item.url),
      state: asOptionalString(item.state),
      isDraft: Boolean(item.isDraft),
      reviewDecision: asOptionalString(item.reviewDecision),
      labels: extractLabelNames(item.labels),
      headRefName: asOptionalString(item.headRefName),
      baseRefName: asOptionalString(item.baseRefName),
      headSha: asOptionalString(item.headRefOid),
      author: extractAuthor(item.author),
      reviewRequests: extractReviewRequestLogins(item.reviewRequests),
    }));
  }

  public async listOpenIssues(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    assignee?: string;
    label?: string;
  }): Promise<GitHubIssueSummary[]> {
    const args = [
      "issue",
      "list",
      "--repo",
      input.repo,
      "--state",
      "open",
      "--limit",
      String(input.limit ?? 30),
    ];
    if (input.assignee) {
      args.push("--assignee", input.assignee);
    }
    if (input.label) {
      args.push("--label", input.label);
    }
    args.push(
      "--json",
      [
        "number",
        "title",
        "body",
        "url",
        "state",
        "author",
        "assignees",
        "labels",
      ].join(","),
    );

    const result = await this.runGh(args, input.cwd);

    return asArray(result.stdout).map((item) => ({
      number: Number(item.number),
      title: String(item.title),
      body: asOptionalString(item.body),
      url: asOptionalString(item.url),
      state: asOptionalString(item.state),
      author: extractAuthor(item.author),
      assignees: extractActorLogins(item.assignees),
      labels: extractLabelNames(item.labels),
    }));
  }

  public async viewIssue(input: {
    repo: string;
    issueNumber: number;
    cwd?: string;
  }): Promise<GitHubIssueDetail> {
    const result = await this.runGh(
      [
        "issue",
        "view",
        String(input.issueNumber),
        "--repo",
        input.repo,
        "--json",
        [
          "number",
          "title",
          "body",
          "url",
          "state",
          "author",
          "assignees",
          "labels",
        ].join(","),
      ],
      input.cwd,
    );
    const parsed = asObject(result.stdout);

    return {
      number: Number(parsed.number),
      title: String(parsed.title),
      body: asOptionalString(parsed.body),
      url: asOptionalString(parsed.url),
      state: asOptionalString(parsed.state),
      author: extractAuthor(parsed.author),
      assignees: extractActorLogins(parsed.assignees),
      labels: extractLabelNames(parsed.labels),
    };
  }

  public async viewPullRequest(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<GitHubPullRequestDetail> {
    const result = await this.runGh(
      [
        "pr",
        "view",
        String(input.prNumber),
        "--repo",
        input.repo,
        "--json",
        [
          "number",
          "title",
          "body",
          "url",
          "state",
          "isDraft",
          "reviewDecision",
          "labels",
          "headRefName",
          "baseRefName",
          "headRefOid",
          "baseRefOid",
          "author",
          "reviewRequests",
          "comments",
          "reviews",
          "statusCheckRollup",
        ].join(","),
      ],
      input.cwd,
    );
    const parsed = asObject(result.stdout);
    const reviewThreads = await this.fetchReviewThreads(input);

    return {
      number: Number(parsed.number),
      title: String(parsed.title),
      body: asOptionalString(parsed.body),
      url: asOptionalString(parsed.url),
      state: asOptionalString(parsed.state),
      isDraft: Boolean(parsed.isDraft),
      reviewDecision: asOptionalString(parsed.reviewDecision),
      labels: extractLabelNames(parsed.labels),
      headRefName: asOptionalString(parsed.headRefName),
      baseRefName: asOptionalString(parsed.baseRefName),
      headSha: asOptionalString(parsed.headRefOid),
      baseSha: asOptionalString(parsed.baseRefOid),
      author: extractAuthor(parsed.author),
      reviewRequests: extractReviewRequestLogins(parsed.reviewRequests),
      comments:
        reviewThreads.length > 0
          ? reviewThreads
          : asArrayValue(parsed.comments),
      reviews: asArrayValue(parsed.reviews),
      checks: asArrayValue(parsed.statusCheckRollup),
    };
  }

  public async resolveReviewThread(input: {
    repo: string;
    threadId: string;
    cwd?: string;
  }): Promise<void> {
    const thread = await this.getReviewThread({
      repo: input.repo,
      threadId: input.threadId,
      cwd: input.cwd,
    });
    if (!thread) {
      throw new ReviewThreadNotFoundError(input.threadId);
    }
    if (thread.isResolved) {
      return;
    }

    const result = await this.runGh(
      [
        "api",
        "graphql",
        "-f",
        `query=${[
          "mutation($threadId: ID!) {",
          "  resolveReviewThread(input: { threadId: $threadId }) {",
          "    thread { id isResolved }",
          "  }",
          "}",
        ].join("\n")}`,
        "-F",
        `threadId=${input.threadId}`,
      ],
      input.cwd,
    );
    const payload = asObject(result.stdout);
    const resolved = payload.data as Record<string, unknown> | undefined;
    const threadData = resolved?.resolveReviewThread as
      | Record<string, unknown>
      | undefined;
    const threadNode = threadData?.thread as
      | Record<string, unknown>
      | undefined;
    if (!threadNode || threadNode.isResolved !== true) {
      throw new Error(`Failed to resolve review thread ${input.threadId}`);
    }
  }

  public async getPullRequestDiff(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<string> {
    const result = await this.runGh(
      ["pr", "diff", String(input.prNumber), "--repo", input.repo],
      input.cwd,
    );
    return result.stdout;
  }

  public async submitReview(input: SubmitReviewInput): Promise<void> {
    const args = [
      "pr",
      "review",
      String(input.prNumber),
      "--repo",
      input.repo,
      `--${input.event.toLowerCase().replace("_", "-")}`,
    ];

    if (input.body) {
      args.push("--body", input.body);
    }

    await this.runGh(args, input.cwd);
  }

  public async addPullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void> {
    if (input.labels.length === 0) {
      return;
    }

    await this.ensureLabelsExist(input.repo, input.labels, input.cwd);
    await this.runGh(
      [
        "api",
        `repos/${input.repo}/issues/${input.prNumber}/labels`,
        "--method",
        "POST",
        ...input.labels.flatMap((label) => ["-f", `labels[]=${label}`]),
      ],
      input.cwd,
    );
  }

  public async removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void> {
    if (input.labels.length === 0) {
      return;
    }

    for (const label of input.labels) {
      try {
        await this.runGh(
          [
            "api",
            `repos/${input.repo}/issues/${input.prNumber}/labels/${encodeURIComponent(label)}`,
            "--method",
            "DELETE",
          ],
          input.cwd,
        );
      } catch (error) {
        if (isMissingPullRequestLabelDelete(error)) {
          continue;
        }
        throw error;
      }
    }
  }

  public async addPullRequestReviewers(input: {
    repo: string;
    prNumber: number;
    reviewers: string[];
    cwd?: string;
  }): Promise<void> {
    if (input.reviewers.length === 0) {
      return;
    }

    await this.runGh(
      [
        "api",
        `repos/${input.repo}/pulls/${input.prNumber}/requested_reviewers`,
        "--method",
        "POST",
        ...input.reviewers.flatMap((reviewer) => [
          "-f",
          `reviewers[]=${reviewer}`,
        ]),
      ],
      input.cwd,
    );
  }

  public async createPullRequest(
    input: CreatePullRequestInput,
  ): Promise<CreatePullRequestResult> {
    const result = await this.runGh(
      [
        "pr",
        "create",
        "--repo",
        input.repo,
        "--head",
        input.headBranch,
        "--base",
        input.baseBranch,
        "--title",
        input.title,
        "--body",
        input.body ?? "",
      ],
      input.cwd,
    );

    const url = result.stdout.trim();
    if (!url) {
      throw new CommandExecutionError("gh pr create returned an empty URL", {
        ...result,
      });
    }

    return {
      url,
      number: parsePrNumberFromUrl(url),
    };
  }

  public async getCurrentUserLogin(input?: {
    cwd?: string;
  }): Promise<string | undefined> {
    const result = await this.runGh(
      ["api", "user", "--jq", ".login"],
      input?.cwd,
    );
    return asOptionalString(result.stdout.trim());
  }

  public async capturePullRequestSnapshot(input: {
    projectId: string;
    repo: string;
    prNumber: number;
    cwd?: string;
    capturedAt?: string;
  }): Promise<PullRequestSnapshotRecord> {
    const [detail, diff] = await Promise.all([
      this.viewPullRequest(input),
      this.getPullRequestDiff(input),
    ]);
    const capturedAt = input.capturedAt ?? this.now().toISOString();

    return {
      id: randomUUID(),
      projectId: input.projectId,
      repo: input.repo,
      prNumber: input.prNumber,
      headSha: detail.headSha ?? "unknown",
      baseSha: detail.baseSha ?? null,
      title: detail.title,
      body: detail.body ?? null,
      author: detail.author ?? null,
      diffRef: `gh:pr-diff:${input.repo}:${input.prNumber}`,
      checksSummary: summarizeChecks(detail.checks),
      unresolvedThreadCount: countUnresolvedThreads(detail.comments),
      reviewState: detail.reviewDecision ?? null,
      payloadJson: JSON.stringify({ detail, diff }),
      capturedAt,
      createdAt: capturedAt,
    };
  }

  private async fetchReviewThreads(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<ReviewThreadComment[]> {
    const { owner, name } = parseRepo(input.repo);
    const result = await this.runGh(
      [
        "api",
        "graphql",
        "-f",
        `query=${[
          "query($owner: String!, $name: String!, $prNumber: Int!) {",
          "  repository(owner: $owner, name: $name) {",
          "    pullRequest(number: $prNumber) {",
          "      reviewThreads(first: 100) {",
          "        nodes {",
          "          id",
          "          isResolved",
          "          comments(first: 1) {",
          "            nodes { id body }",
          "          }",
          "        }",
          "      }",
          "    }",
          "  }",
          "}",
        ].join("\n")}`,
        "-F",
        `owner=${owner}`,
        "-F",
        `name=${name}`,
        "-F",
        `prNumber=${input.prNumber}`,
      ],
      input.cwd,
    );

    const payload = asObject(result.stdout);
    const repository = (payload.data as Record<string, unknown> | undefined)
      ?.repository as Record<string, unknown> | undefined;
    const pullRequest = repository?.pullRequest as
      | Record<string, unknown>
      | undefined;
    const reviewThreads = pullRequest?.reviewThreads as
      | Record<string, unknown>
      | undefined;
    return asArrayValue(reviewThreads?.nodes)
      .map((node) => normalizeReviewThread(node))
      .filter((value): value is ReviewThreadComment => Boolean(value));
  }

  private async getReviewThread(input: {
    repo: string;
    threadId: string;
    cwd?: string;
  }): Promise<ReviewThreadNode | null> {
    const result = await this.runGh(
      [
        "api",
        "graphql",
        "-f",
        `query=${[
          "query($threadId: ID!) {",
          "  node(id: $threadId) {",
          "    ... on PullRequestReviewThread {",
          "      id",
          "      isResolved",
          "    }",
          "  }",
          "}",
        ].join("\n")}`,
        "-F",
        `threadId=${input.threadId}`,
      ],
      input.cwd,
    );
    const payload = asObject(result.stdout);
    const node = (payload.data as Record<string, unknown> | undefined)?.node as
      | Record<string, unknown>
      | undefined;
    const id = asOptionalString(node?.id);
    if (!id) {
      return null;
    }

    return {
      id,
      isResolved: node?.isResolved === true,
    };
  }

  private async runGh(args: string[], cwd?: string) {
    return runCommand({
      command: this.options.ghPath,
      args,
      cwd: cwd ?? this.options.cwd,
    });
  }

  private async ensureLabelsExist(
    repo: string,
    labels: string[],
    cwd?: string,
  ): Promise<void> {
    const uniqueLabels = labels.filter(
      (label, index, values) => values.indexOf(label) === index,
    );

    for (const label of uniqueLabels) {
      await this.runGh(
        [
          "label",
          "create",
          label,
          "--repo",
          repo,
          "--color",
          resolveLabelColor(label),
          "--description",
          resolveLabelDescription(label),
          "--force",
        ],
        cwd,
      );
    }
  }
}

interface ReviewThreadNode {
  id: string;
  isResolved: boolean;
}

interface ReviewThreadComment {
  id: string;
  threadId: string;
  state: "RESOLVED" | "UNRESOLVED";
  isResolved: boolean;
  body?: string;
}

function summarizeChecks(checks: unknown[]): string | null {
  if (checks.length === 0) {
    return null;
  }

  const states = checks
    .map(
      (check) =>
        asOptionalString((check as Record<string, unknown>).conclusion) ??
        asOptionalString((check as Record<string, unknown>).state) ??
        "unknown",
    )
    .join(", ");

  return states;
}

function countUnresolvedThreads(comments: unknown[]): number {
  return comments.filter((comment) => {
    const state = asOptionalString(
      (comment as Record<string, unknown>).state ??
        (comment as Record<string, unknown>).isResolved,
    );
    return state !== "RESOLVED" && state !== "true";
  }).length;
}

function normalizeReviewThread(value: unknown): ReviewThreadComment | null {
  if (!value || typeof value !== "object") {
    return null;
  }

  const row = value as Record<string, unknown>;
  const threadId = asOptionalString(row.id);
  if (!threadId) {
    return null;
  }

  const commentNode = asArrayValue(
    (row.comments as Record<string, unknown> | undefined)?.nodes,
  )[0] as Record<string, unknown> | undefined;
  const commentId = asOptionalString(commentNode?.id) ?? threadId;
  const isResolved = row.isResolved === true;

  return {
    id: commentId,
    threadId,
    state: isResolved ? "RESOLVED" : "UNRESOLVED",
    isResolved,
    body: asOptionalString(commentNode?.body),
  };
}

function parseRepo(repo: string): { owner: string; name: string } {
  const [owner, name] = repo.split("/");
  if (!owner || !name) {
    throw new Error(`Invalid GitHub repo: ${repo}`);
  }
  return { owner, name };
}

function resolveLabelColor(label: string): string {
  switch (label.trim().toLowerCase()) {
    case "looper:plan":
      return "5319e7";
    case "looper:spec-reviewing":
      return "1d76db";
    case "looper:spec-ready":
      return "0e8a16";
    case "looper:needs-human":
      return "d93f0b";
    default:
      return "5319e7";
  }
}

function resolveLabelDescription(label: string): string {
  switch (label.trim().toLowerCase()) {
    case "looper:plan":
      return "Picked up automatically by planner";
    case "looper:spec-reviewing":
      return "Spec PR is under review";
    case "looper:spec-ready":
      return "Spec PR is ready for implementation";
    case "looper:needs-human":
      return "Looper requires manual intervention";
    default:
      return "Managed by looper";
  }
}

function asObject(value: string): Record<string, unknown> {
  try {
    return JSON.parse(value) as Record<string, unknown>;
  } catch (error) {
    throw new CommandExecutionError("Invalid gh JSON payload", {
      exitCode: 0,
      stdout: value,
      stderr: error instanceof Error ? error.message : "Unknown parse error",
      durationMs: 0,
    });
  }
}

function asArray(value: string): Record<string, unknown>[] {
  const parsed = asObjectArray(value);
  return parsed;
}

function asObjectArray(value: string): Record<string, unknown>[] {
  try {
    const parsed = JSON.parse(value);
    return Array.isArray(parsed) ? (parsed as Record<string, unknown>[]) : [];
  } catch (error) {
    throw new CommandExecutionError("Invalid gh JSON payload", {
      exitCode: 0,
      stdout: value,
      stderr: error instanceof Error ? error.message : "Unknown parse error",
      durationMs: 0,
    });
  }
}

function asOptionalString(value: unknown): string | undefined {
  if (typeof value !== "string" || value.length === 0) {
    return undefined;
  }

  return value;
}

function asArrayValue(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function extractAuthor(value: unknown): string | undefined {
  if (!value || typeof value !== "object") {
    return undefined;
  }

  const author = value as Record<string, unknown>;
  return asOptionalString(author.login) ?? asOptionalString(author.name);
}

function extractReviewRequestLogins(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }

  return value
    .map(extractReviewRequestLogin)
    .filter((login): login is string => Boolean(login));
}

function extractReviewRequestLogin(value: unknown): string | undefined {
  if (!value || typeof value !== "object") {
    return undefined;
  }

  const entry = value as Record<string, unknown>;
  const directType = asOptionalString(entry.__typename);
  const directLogin = asOptionalString(entry.login);
  if (directType === "User" && directLogin) {
    return directLogin;
  }

  const requestedReviewer = entry.requestedReviewer;
  if (!requestedReviewer || typeof requestedReviewer !== "object") {
    return undefined;
  }

  const reviewer = requestedReviewer as Record<string, unknown>;
  const reviewerType = asOptionalString(reviewer.__typename);
  const reviewerLogin = asOptionalString(reviewer.login);
  if (reviewerType === "User" && reviewerLogin) {
    return reviewerLogin;
  }

  return undefined;
}

function extractActorLogins(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }

  return value
    .map((item) => extractAuthor(item))
    .filter((login): login is string => Boolean(login));
}

function extractLabelNames(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }

  return value
    .map((item) => {
      if (!item || typeof item !== "object") {
        return undefined;
      }
      return asOptionalString((item as Record<string, unknown>).name);
    })
    .filter((name): name is string => Boolean(name));
}

function parsePrNumberFromUrl(url: string): number | undefined {
  const match = url.match(/\/pull\/(\d+)(?:\/|$)/);
  if (!match) {
    return undefined;
  }

  const value = Number(match[1]);
  return Number.isInteger(value) && value > 0 ? value : undefined;
}

function isMissingPullRequestLabelDelete(error: unknown): boolean {
  if (!(error instanceof CommandExecutionError)) {
    return false;
  }

  const output = `${error.result.stdout}\n${error.result.stderr}`.toLowerCase();
  return output.includes("404") && output.includes("label");
}
