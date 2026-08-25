export type ReviewFixHandoffKind =
  | "review_fix_budget"
  | "review_scope_human";

export type ReviewFixHandoff = {
  kind: ReviewFixHandoffKind;
  hitlEnabled: boolean;
  resume: string;
  nextAction: string;
  unpauseCommand: string;
  stopCommand: string;
  head?: string;
  lastReviewedSignalFingerprint?: string;
  reviewerCount?: number;
  fixerCount?: number;
  exhaustedBy?: string;
  question?: string;
  evidence?: string;
  pauseReason?: string;
};

type LoopLike = {
  seq: number;
  status?: string | null;
  type?: string | null;
  metadataJson?: string | null;
};

type hitlAsk = {
  kind?: string;
  question?: string;
  status?: string;
};

type loopMetadata = {
  hitl?: hitlAsk;
  pauseReason?: string;
  lastPublishedHeadSha?: string;
  lastFixHeadSha?: string;
  lastReviewedSignalFingerprint?: string;
  loop?: { iterationCount?: number };
  reviewFixBudget?: {
    pushCount?: number;
    exhaustedBy?: string;
    pauseReason?: string;
  };
  reviewScopeHuman?: {
    question?: string;
    evidence?: string;
    pauseReason?: string;
  };
};
const BUDGET_REASONS: Record<string, true> = {
  review_fix_budget_exhausted: true,
  sibling_review_fix_budget: true,
};
const SCOPE_REASONS: Record<string, true> = {
  review_scope_human_required: true,
  sibling_review_scope_human: true,
};

function parseMetadata(raw?: string | null): loopMetadata {
  if (!raw?.trim()) return {};
  try {
    return JSON.parse(raw) as loopMetadata;
  } catch {
    return {};
  }
}

function awaitingAsk(ask: hitlAsk | undefined, kind: ReviewFixHandoffKind): boolean {
  return (
    (ask?.kind ?? "").trim() === kind &&
    (ask?.status ?? "").trim().toLowerCase() === "awaiting"
  );
}

export function readReviewFixHandoff(loop: LoopLike): ReviewFixHandoff | null {
  const meta = parseMetadata(loop.metadataJson);
  const status = (loop.status ?? "").trim().toLowerCase();
  const pauseReason = (meta.pauseReason ?? "").trim();
  const budgetAsk = awaitingAsk(meta.hitl, "review_fix_budget");
  const scopeAsk = awaitingAsk(meta.hitl, "review_scope_human");
  const budgetPaused =
    status === "paused" &&
    (BUDGET_REASONS[pauseReason] === true ||
      BUDGET_REASONS[(meta.reviewFixBudget?.pauseReason ?? "").trim()] === true);
  const scopePaused =
    status === "paused" &&
    (SCOPE_REASONS[pauseReason] === true ||
      SCOPE_REASONS[(meta.reviewScopeHuman?.pauseReason ?? "").trim()] === true);

  const isBudget = budgetAsk || budgetPaused;
  const isScope = !isBudget && (scopeAsk || scopePaused);
  if (!isBudget && !isScope) return null;

  const kind: ReviewFixHandoffKind = isBudget
    ? "review_fix_budget"
    : "review_scope_human";
  const hitlEnabled = kind === "review_fix_budget" ? budgetAsk : scopeAsk;
  const selector = String(loop.seq);
  const unpauseCommand = `looper unpause ${selector}`;
  const stopCommand = `looper stop ${selector}`;
  const resume = `${unpauseCommand} / ${stopCommand}`;
  const nextAction = hitlEnabled
    ? `Continue / Stop; ${resume}`
    : resume;
  const head =
    (meta.lastPublishedHeadSha ?? "").trim() ||
    (meta.lastFixHeadSha ?? "").trim() ||
    undefined;
  const question =
    (meta.hitl?.question ?? "").trim() ||
    (meta.reviewScopeHuman?.question ?? "").trim() ||
    undefined;
  const evidence = (meta.reviewScopeHuman?.evidence ?? "").trim() || undefined;

  return {
    kind,
    hitlEnabled,
    resume,
    nextAction,
    unpauseCommand,
    stopCommand,
    head,
    lastReviewedSignalFingerprint:
      (meta.lastReviewedSignalFingerprint ?? "").trim() || undefined,
    reviewerCount:
      typeof meta.loop?.iterationCount === "number"
        ? meta.loop.iterationCount
        : undefined,
    fixerCount:
      typeof meta.reviewFixBudget?.pushCount === "number"
        ? meta.reviewFixBudget.pushCount
        : undefined,
    exhaustedBy: (meta.reviewFixBudget?.exhaustedBy ?? "").trim() || undefined,
    question,
    evidence,
    pauseReason:
      pauseReason ||
      (meta.reviewFixBudget?.pauseReason ?? "").trim() ||
      (meta.reviewScopeHuman?.pauseReason ?? "").trim() ||
      undefined,
  };
}

export function reviewFixHandoffTitle(handoff: ReviewFixHandoff): string {
  if (handoff.kind === "review_scope_human") {
    return "Human scope decision required";
  }
  return "Review-fix budget exhausted";
}
