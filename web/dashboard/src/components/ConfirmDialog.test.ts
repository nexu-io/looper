import { describe, expect, it } from "vitest";
import { classifyRetryWorktree } from "@/components/LoopActionBar";

/**
 * Confirm-gated actions for LoopActionBar.
 * Dirty managed retry opens discard confirm; clean retry does not.
 */
function actionRequiresConfirm(
  action: "pause" | "unpause" | "retry" | "stop" | "takeover" | "handback",
  opts?: { worktreeDirty?: boolean; managed?: boolean },
): boolean {
  if (action === "stop" || action === "takeover" || action === "handback") {
    return true;
  }
  if (action === "retry") {
    return (
      classifyRetryWorktree({
        loopId: "x",
        seq: 1,
        present: true,
        managed: opts?.managed ?? true,
        dirty: opts?.worktreeDirty ?? false,
      }) === "offer-discard"
    );
  }
  return false;
}

describe("actionRequiresConfirm", () => {
  it("requires confirm for stop, takeover, handback", () => {
    expect(actionRequiresConfirm("pause")).toBe(false);
    expect(actionRequiresConfirm("unpause")).toBe(false);
    expect(actionRequiresConfirm("retry")).toBe(false);
    expect(actionRequiresConfirm("stop")).toBe(true);
    expect(actionRequiresConfirm("takeover")).toBe(true);
    expect(actionRequiresConfirm("handback")).toBe(true);
  });

  it("requires confirm for retry only when managed worktree is dirty", () => {
    expect(actionRequiresConfirm("retry", { worktreeDirty: false })).toBe(
      false,
    );
    expect(actionRequiresConfirm("retry", { worktreeDirty: true })).toBe(true);
    expect(
      actionRequiresConfirm("retry", { worktreeDirty: true, managed: false }),
    ).toBe(false);
  });
});
