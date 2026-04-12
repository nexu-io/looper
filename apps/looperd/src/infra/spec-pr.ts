import type { GitHubPullRequestDetail } from "./github";

export const SPEC_REVIEWING_LABEL = "looper:spec-reviewing";
export const SPEC_READY_LABEL = "looper:spec-ready";
export const NEEDS_HUMAN_LABEL = "looper:needs-human";

export type PullRequestPhase = "spec" | "implementation";

export function normalizeLabel(label: string): string {
  return label.trim().toLowerCase();
}

export function hasLabel(labels: string[] | undefined, label: string): boolean {
  const normalizedTarget = normalizeLabel(label);
  return (labels ?? []).some(
    (candidate) => normalizeLabel(candidate) === normalizedTarget,
  );
}

export function resolvePullRequestPhase(input: {
  labels?: string[];
}): PullRequestPhase {
  return hasLabel(input.labels, SPEC_REVIEWING_LABEL)
    ? "spec"
    : "implementation";
}

export function parseSpecPathFromPullRequestBody(
  body: string | null | undefined,
): string | null {
  if (!body) {
    return null;
  }

  const match = /^Spec:\s*(.+)$/im.exec(body);
  return match?.[1]?.trim() || null;
}

export function countUnresolvedReviewThreads(comments: unknown[]): number {
  return comments.filter((comment) => {
    if (!comment || typeof comment !== "object") {
      return false;
    }

    const record = comment as Record<string, unknown>;
    const state = record.state;
    if (typeof state === "string") {
      return state.toUpperCase() !== "RESOLVED";
    }

    return record.isResolved !== true;
  }).length;
}

export function isSpecReviewClean(
  detail: Pick<GitHubPullRequestDetail, "comments" | "reviewDecision">,
): boolean {
  return (
    countUnresolvedReviewThreads(detail.comments) === 0 &&
    detail.reviewDecision?.toUpperCase() !== "CHANGES_REQUESTED"
  );
}
