import { describe, expect, it } from "vitest";

/**
 * Pure helper for confirm-gated actions (stop / takeover / handback).
 * UI ConfirmDialog is presentational; this freezes which actions require confirm.
 */
function actionRequiresConfirm(
  action: "pause" | "unpause" | "retry" | "stop" | "takeover" | "handback",
): boolean {
  return action === "stop" || action === "takeover" || action === "handback";
}

describe("actionRequiresConfirm", () => {
  it("requires confirm only for stop, takeover, handback", () => {
    expect(actionRequiresConfirm("pause")).toBe(false);
    expect(actionRequiresConfirm("unpause")).toBe(false);
    expect(actionRequiresConfirm("retry")).toBe(false);
    expect(actionRequiresConfirm("stop")).toBe(true);
    expect(actionRequiresConfirm("takeover")).toBe(true);
    expect(actionRequiresConfirm("handback")).toBe(true);
  });
});
