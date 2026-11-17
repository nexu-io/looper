import { describe, expect, it } from "vitest";
import { readReviewFixHandoff } from "@/lib/reviewFixHandoff";

describe("readReviewFixHandoff", () => {
  it("builds current-loop no-HITL budget commands", () => {
    const handoff = readReviewFixHandoff({
      seq: 12,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "review_fix_budget_exhausted",
        reviewFixBudget: {
          exhaustedBy: "reviewer",
          pauseReason: "review_fix_budget_exhausted",
        },
      }),
    });
    expect(handoff).toMatchObject({
      kind: "review_fix_budget",
      pauseReason: "review_fix_budget_exhausted",
      unpauseCommand: "looper unpause 12",
      stopCommand: "looper stop 12",
      title: "Review-fix budget exhausted",
    });
  });

  it("shows HITL budget unpause/stop without requiring Continue/Stop on the card", () => {
    const handoff = readReviewFixHandoff({
      seq: 4,
      status: "awaiting_human",
      metadataJson: JSON.stringify({
        hitl: {
          kind: "review_fix_budget",
          status: "awaiting",
          question: "reviewer hit its review-fix budget on acme/looper#42 (3/3).",
        },
      }),
    });
    expect(handoff?.kind).toBe("review_fix_budget");
    expect(handoff?.unpauseCommand).toBe("looper unpause 4");
    expect(handoff?.stopCommand).toBe("looper stop 4");
    expect(handoff?.unpauseCommand).not.toContain("Continue");
    expect(handoff?.stopCommand).not.toContain("Stop");
  });

  it("surfaces current-loop scope hold reason", () => {
    const handoff = readReviewFixHandoff({
      seq: 9,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "review_scope_human_required",
        reviewScopeHuman: {
          heldBy: "reviewer",
          question: "Clarify AGENTS.md vs PR non-goals before unpause.",
          evidence: "thread PRRT_1 conflicts with stated non-goal",
          pauseReason: "review_scope_human_required",
        },
      }),
    });
    expect(handoff).toMatchObject({
      kind: "review_scope_human",
      pauseReason: "review_scope_human_required",
      unpauseCommand: "looper unpause 9",
      stopCommand: "looper stop 9",
      title: "Human scope decision required",
    });
  });

  it("does not promise resume when budget and scope holds coexist", () => {
    const handoff = readReviewFixHandoff({
      seq: 12,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "sibling_review_fix_budget",
        reviewFixBudget: { pauseReason: "sibling_review_fix_budget" },
        reviewScopeHuman: {
          heldBy: "reviewer",
          pauseReason: "sibling_review_scope_human",
        },
      }),
    });
    expect(handoff?.lead).toContain("releases the budget hold");
    expect(handoff?.lead).toContain("other holds, including scope holds");
    expect(handoff?.lead).not.toContain("resumes the pair");
  });

  it("explains that an unrelated agent ask survives scope release", () => {
    const handoff = readReviewFixHandoff({
      seq: 12,
      status: "awaiting_human",
      metadataJson: JSON.stringify({
        hitl: { kind: "agent_question", status: "awaiting" },
        reviewScopeHuman: { heldBy: "reviewer" },
      }),
    });
    expect(handoff?.kind).toBe("review_scope_human");
    expect(handoff?.lead).toContain("unanswered agent/HITL asks");
    expect(handoff?.lead).toContain("such as");
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

  it("ignores pending-only scope evidence", () => {
    expect(
      readReviewFixHandoff({
        seq: 9,
        status: "paused",
        metadataJson: JSON.stringify({
          reviewScopeHuman: {
            pending: true,
            question: "Clarify AGENTS.md vs PR non-goals before unpause.",
            evidence: "thread PRRT_1 conflicts with stated non-goal",
          },
        }),
      }),
    ).toBeNull();
  });

  it("uses distinct lead text for budget vs scope", () => {
    const budget = readReviewFixHandoff({
      seq: 12,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "review_fix_budget_exhausted",
        reviewFixBudget: { pauseReason: "review_fix_budget_exhausted" },
      }),
    });
    expect(budget?.lead).toContain("refills only exhausted meters");
    expect(budget?.lead).toContain("other holds, including scope holds");
    expect(budget?.lead).not.toContain("resumes the pair");
    expect(budget?.lead).toContain("exhaustion is not approval");
    expect(budget?.lead).not.toContain("releases the scope hold");

    const scope = readReviewFixHandoff({
      seq: 9,
      status: "paused",
      metadataJson: JSON.stringify({
        pauseReason: "review_scope_human_required",
        reviewScopeHuman: { pauseReason: "review_scope_human_required" },
      }),
    });
    expect(scope?.lead).toContain("releases the scope hold");
    expect(scope?.lead).toContain("failed");
    expect(scope?.lead).toContain("interrupted");
    expect(scope?.lead).toContain("human takeover");
    expect(scope?.lead).toContain("manual pause");
    expect(scope?.lead).not.toContain("resumes the pair");
  });
});
