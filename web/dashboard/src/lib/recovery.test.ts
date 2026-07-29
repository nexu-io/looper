import { describe, expect, it } from "vitest";
import type { LoopWorktreeStatus } from "@/lib/api";
import {
  classifyRecoveryWorktree,
  recoveryOffersDiscard,
  recoveryRecommendsRetry,
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

describe("classifyRecoveryWorktree", () => {
  it("recommends retry for clean managed worktree", () => {
    const mode = classifyRecoveryWorktree(
      wt({
        present: true,
        managed: true,
        dirty: false,
        clean: true,
        reason: "already_clean",
      }),
    );
    expect(mode).toBe("clean");
    expect(recoveryRecommendsRetry(mode)).toBe(true);
    expect(recoveryOffersDiscard(mode)).toBe(false);
  });

  it("offers discard only for managed dirty worktree", () => {
    const mode = classifyRecoveryWorktree(
      wt({
        present: true,
        managed: true,
        dirty: true,
        clean: false,
        reason: "dirty",
        worktreePath: "/tmp/wt",
      }),
    );
    expect(mode).toBe("managed_dirty");
    expect(recoveryOffersDiscard(mode)).toBe(true);
    expect(recoveryRecommendsRetry(mode)).toBe(false);
  });

  it("never offers discard for unmanaged dirty worktree", () => {
    const mode = classifyRecoveryWorktree(
      wt({
        present: true,
        managed: false,
        dirty: true,
        reason: "unmanaged",
        worktreePath: "/tmp/repo",
      }),
    );
    expect(mode).toBe("unmanaged_or_unverifiable");
    expect(recoveryOffersDiscard(mode)).toBe(false);
  });

  it("never offers discard when dirty state is unverifiable", () => {
    const mode = classifyRecoveryWorktree(
      wt({
        present: true,
        managed: true,
        reason: "status_unavailable",
        worktreePath: "/tmp/wt",
      }),
    );
    expect(mode).toBe("unmanaged_or_unverifiable");
    expect(recoveryOffersDiscard(mode)).toBe(false);
  });

  it("is unclassifiable when worktree is missing or fetch fails", () => {
    expect(
      classifyRecoveryWorktree(
        wt({ present: false, managed: true, reason: "worktree_missing" }),
      ),
    ).toBe("unclassifiable");
    expect(classifyRecoveryWorktree(null, { fetchFailed: true })).toBe(
      "unclassifiable",
    );
    expect(classifyRecoveryWorktree(null)).toBe("unclassifiable");
    expect(
      classifyRecoveryWorktree(
        wt({
          present: false,
          managed: false,
          reason: "loop_type_without_worktree",
        }),
      ),
    ).toBe("unclassifiable");
  });
});
