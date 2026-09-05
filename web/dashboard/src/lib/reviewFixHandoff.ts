export type ReviewFixHandoffKind = "review_fix_budget" | "review_scope_human";

export type ReviewFixHandoff = {
  kind: ReviewFixHandoffKind;
  pauseReason: string;
  unpauseCommand: string;
  stopCommand: string;
  title: string;
  lead: string;
};

type LoopLike = {
  seq: number;
  id?: string;
  status?: string | null;
  metadataJson?: string | null;
};

type LoopMetadata = {
  hitl?: { kind?: string; status?: string };
  pauseReason?: string;
  reviewFixBudget?: { pauseReason?: string; exhaustedBy?: string };
  reviewScopeHuman?: { pauseReason?: string; heldBy?: string };
};

const BUDGET_REASONS: Record<string, true> = {
  sibling_review_fix_budget: true,
  review_fix_budget_exhausted: true,
};

const SCOPE_REASONS: Record<string, true> = {
  sibling_review_scope_human: true,
  review_scope_human_required: true,
};

const BUDGET_LEAD =
  "Continue/Unpause refills only exhausted meters and resumes the pair; Stop terminates both; exhaustion is not approval.";

const SCOPE_LEAD =
  "Continue/Unpause releases the scope hold; independent blockers (failed, interrupted, human takeover, or a manual pause) remain; Stop terminates both.";

function parseMetadata(raw?: string | null): LoopMetadata {
  if (!raw?.trim()) return {};
  try {
    return JSON.parse(raw) as LoopMetadata;
  } catch {
    return {};
  }
}

function isReviewFixBudgetHold(status: string, meta: LoopMetadata): boolean {
  if (status === "awaiting_human") {
    return (meta.hitl?.kind ?? "").trim() === "review_fix_budget";
  }
  if (status !== "paused") return false;
  const top = (meta.pauseReason ?? "").trim();
  const nested = (meta.reviewFixBudget?.pauseReason ?? "").trim();
  return (
    BUDGET_REASONS[top] === true ||
    BUDGET_REASONS[nested] === true ||
    (meta.reviewFixBudget?.exhaustedBy ?? "").trim() !== ""
  );
}

function isReviewScopeHumanHold(status: string, meta: LoopMetadata): boolean {
  if (status === "terminated" || status === "stopped" || status === "completed") {
    return false;
  }
  if ((meta.reviewScopeHuman?.heldBy ?? "").trim()) return true;
  const nested = (meta.reviewScopeHuman?.pauseReason ?? "").trim();
  const top = (meta.pauseReason ?? "").trim();
  if (SCOPE_REASONS[nested] === true || SCOPE_REASONS[top] === true) return true;
  return (
    status === "awaiting_human" &&
    (meta.hitl?.kind ?? "").trim() === "review_scope_human"
  );
}

export function readReviewFixHandoff(loop: LoopLike): ReviewFixHandoff | null {
  const meta = parseMetadata(loop.metadataJson);
  const status = (loop.status ?? "").trim();
  const budget = isReviewFixBudgetHold(status, meta);
  if (!budget && !isReviewScopeHumanHold(status, meta)) return null;
  const kind: ReviewFixHandoffKind = budget
    ? "review_fix_budget"
    : "review_scope_human";
  const selector =
    loop.seq !== 0 ? String(loop.seq) : (loop.id ?? "").trim() || "0";
  return {
    kind,
    pauseReason:
      (meta.pauseReason ?? "").trim() ||
      (meta.reviewFixBudget?.pauseReason ?? "").trim() ||
      (meta.reviewScopeHuman?.pauseReason ?? "").trim(),
    unpauseCommand: `looper unpause ${selector}`,
    stopCommand: `looper stop ${selector}`,
    title:
      kind === "review_fix_budget"
        ? "Review-fix budget exhausted"
        : "Human scope decision required",
    lead: kind === "review_fix_budget" ? BUDGET_LEAD : SCOPE_LEAD,
  };
}
