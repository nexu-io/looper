import { describe, expect, it } from "vitest";
import type { LoopWorktreeStatus } from "@/lib/api";
import {
  classifyRetryWorktree,
  recoveryOffersClear,
  recoveryOffersDiscard,
  recoveryRecommendsRetry,
  recoveryWorktreeDecision,
  shouldShowRecoveryCard,
} from "@/lib/recovery";

function wt(
  partial: Partial<LoopWorktreeStatus> &
    Pick<LoopWorktreeStatus, "present" | "managed">,
): LoopWorktreeStatus {
  return {
    loopId: "loop_1",
    seq: 1,
    ...partial,
  };
}

describe("shouldShowRecoveryCard", () => {
  it("shows only for displayStatus=manual_intervention", () => {
    expect(
      shouldShowRecoveryCard({
        status: "paused",
        displayStatus: "manual_intervention",
      }),
    ).toBe(true);
    expect(
      shouldShowRecoveryCard({ status: "failed", displayStatus: "failed" }),
    ).toBe(false);
  });

  it("never mixes with awaiting_human decision workflow", () => {
    expect(
      shouldShowRecoveryCard({
        status: "awaiting_human",
        displayStatus: "awaiting_human",
      }),
    ).toBe(false);
    expect(
      shouldShowRecoveryCard({
        status: "awaiting_human",
        displayStatus: "manual_intervention",
      }),
    ).toBe(false);
  });
});

describe("recovery worktree decisions (shared classifyRetryWorktree)", () => {
  it("recommends retry for clean managed worktree", () => {
    const tree = wt({
      present: true,
      managed: true,
      dirty: false,
      clean: true,
      reason: "already_clean",
    });
    expect(classifyRetryWorktree(tree)).toBe("ok");
    expect(recoveryWorktreeDecision(tree)).toBe("ok");
    expect(recoveryRecommendsRetry(tree)).toBe(true);
    expect(recoveryOffersDiscard(tree)).toBe(false);
  });

  it("offers discard only for managed dirty worktree", () => {
    const tree = wt({
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      reason: "dirty",
      worktreePath: "/tmp/wt",
    });
    expect(classifyRetryWorktree(tree)).toBe("offer-discard");
    expect(recoveryWorktreeDecision(tree)).toBe("offer-discard");
    expect(recoveryOffersDiscard(tree)).toBe(true);
    expect(recoveryRecommendsRetry(tree)).toBe(false);
  });

  it("never offers discard for unmanaged dirty worktree", () => {
    const tree = wt({
      present: true,
      managed: false,
      dirty: true,
      reason: "unmanaged",
      worktreePath: "/tmp/repo",
    });
    expect(classifyRetryWorktree(tree)).toBe("inspect-only");
    expect(recoveryWorktreeDecision(tree)).toBe("inspect-only");
    expect(recoveryOffersDiscard(tree)).toBe(false);
  });

  it("never offers discard when dirty state is unverifiable", () => {
    const tree = wt({
      present: true,
      managed: true,
      reason: "status_unavailable",
      worktreePath: "/tmp/wt",
    });
    expect(classifyRetryWorktree(tree)).toBe("inspect-only");
    expect(recoveryWorktreeDecision(tree)).toBe("inspect-only");
    expect(recoveryOffersDiscard(tree)).toBe(false);
    expect(recoveryOffersClear(tree)).toBe(false);
    expect(recoveryRecommendsRetry(tree)).toBe(false);
  });

  it("offers clear for managed unusable_path leftovers", () => {
    const tree = wt({
      present: true,
      managed: true,
      reason: "unusable_path",
      worktreePath: "/tmp/hollow-wt",
      supportsClearUnusablePath: true,
    });
    expect(classifyRetryWorktree(tree)).toBe("offer-clear");
    expect(recoveryWorktreeDecision(tree)).toBe("offer-clear");
    expect(recoveryOffersClear(tree)).toBe(true);
    expect(recoveryOffersDiscard(tree)).toBe(false);
    expect(recoveryRecommendsRetry(tree)).toBe(false);
  });

  it("does not offer clear when daemon omits supportsClearUnusablePath", () => {
    const tree = wt({
      present: true,
      managed: true,
      reason: "unusable_path",
      worktreePath: "/tmp/hollow-wt",
    });
    expect(classifyRetryWorktree(tree)).toBe("inspect-only");
    expect(recoveryOffersClear(tree)).toBe(false);
  });

  it("recommends retry for legitimate missing-worktree preflight", () => {
    const missing = wt({
      present: false,
      managed: true,
      reason: "worktree_missing",
    });
    const noTree = wt({
      present: false,
      managed: false,
      reason: "no_worktree",
    });
    const noWorktreeType = wt({
      present: false,
      managed: false,
      reason: "loop_type_without_worktree",
    });
    expect(classifyRetryWorktree(missing)).toBe("ok");
    expect(recoveryWorktreeDecision(missing)).toBe("ok");
    expect(recoveryRecommendsRetry(missing)).toBe(true);
    expect(recoveryOffersDiscard(missing)).toBe(false);
    expect(recoveryWorktreeDecision(noTree)).toBe("ok");
    expect(recoveryRecommendsRetry(noTree)).toBe(true);
    expect(recoveryWorktreeDecision(noWorktreeType)).toBe("ok");
  });

  it("is unavailable only when preflight fetch fails or payload is missing", () => {
    expect(recoveryWorktreeDecision(null, { fetchFailed: true })).toBeNull();
    expect(recoveryWorktreeDecision(null)).toBeNull();
    expect(recoveryOffersDiscard(null, { fetchFailed: true })).toBe(false);
    expect(recoveryRecommendsRetry(null, { fetchFailed: true })).toBe(false);
  });
});
