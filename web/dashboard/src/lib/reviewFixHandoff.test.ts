import { describe, expect, it } from "vitest";
import {
  readReviewFixHandoff,
  reviewFixHandoffTitle,
} from "@/lib/reviewFixHandoff";

describe("readReviewFixHandoff", () => {
  it("builds a no-HITL budget brief with exact resume commands", () => {
    const handoff = readReviewFixHandoff({
      seq: 12,
      status: "paused",
      type: "reviewer",
      metadataJson: JSON.stringify({
        pauseReason: "review_fix_budget_exhausted",
        lastPublishedHeadSha: "abc123",
        lastReviewedSignalFingerprint: "sig-abc",
        loop: { iterationCount: 3 },
        reviewFixBudget: {
          exhaustedBy: "reviewer",
          pauseReason: "review_fix_budget_exhausted",
        },
      }),
    });
    expect(handoff).toMatchObject({
      kind: "review_fix_budget",
      hitlEnabled: false,
      unpauseCommand: "looper unpause 12",
      stopCommand: "looper stop 12",
      resume: "looper unpause 12 / looper stop 12",
      nextAction: "looper unpause 12 / looper stop 12",
      head: "abc123",
      lastReviewedSignalFingerprint: "sig-abc",
      reviewerCount: 3,
      exhaustedBy: "reviewer",
    });
    expect(reviewFixHandoffTitle(handoff!)).toBe("Review-fix budget exhausted");
  });

  it("keeps HITL budget asks on Continue/Stop without implying approval", () => {
    const handoff = readReviewFixHandoff({
      seq: 4,
      status: "awaiting_human",
      metadataJson: JSON.stringify({
        hitl: {
          kind: "review_fix_budget",
          status: "awaiting",
          question: "reviewer hit its review-fix budget on acme/looper#42 (3/3).",
        },
        loop: { iterationCount: 3 },
      }),
    });
    expect(handoff?.hitlEnabled).toBe(true);
    expect(handoff?.nextAction).toContain("Continue / Stop");
    expect(handoff?.nextAction).toContain("looper unpause 4");
    expect(handoff?.question).toContain("review-fix budget");
  });

  it("surfaces a no-HITL scope hold with evidence", () => {
    const handoff = readReviewFixHandoff({
      seq: 9,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "review_scope_human_required",
        reviewScopeHuman: {
          question: "Clarify AGENTS.md vs PR non-goals before unpause.",
          evidence: "thread PRRT_1 conflicts with stated non-goal",
          pauseReason: "review_scope_human_required",
        },
      }),
    });
    expect(handoff).toMatchObject({
      kind: "review_scope_human",
      hitlEnabled: false,
      question: "Clarify AGENTS.md vs PR non-goals before unpause.",
      evidence: "thread PRRT_1 conflicts with stated non-goal",
      resume: "looper unpause 9 / looper stop 9",
    });
    expect(reviewFixHandoffTitle(handoff!)).toBe(
      "Human scope decision required",
    );
  });

  it("ignores ordinary paused loops", () => {
    expect(
      readReviewFixHandoff({
        seq: 1,
        status: "paused",
        metadataJson: JSON.stringify({ pauseReason: "manual" }),
      }),
    ).toBeNull();
  });
});
