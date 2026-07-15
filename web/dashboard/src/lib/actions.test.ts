import { describe, expect, it } from "vitest";
import { actionsForLoopStatus, type LoopAction } from "./actions";

const ALL: LoopAction[] = [
  "pause",
  "unpause",
  "retry",
  "stop",
  "takeover",
  "handback",
];

function enabledOf(
  status: string,
  opts?: { hasActiveRun?: boolean },
): LoopAction[] {
  const m = actionsForLoopStatus(status, opts);
  return ALL.filter((a) => m[a]);
}

describe("actionsForLoopStatus", () => {
  it("normalizes case and trims", () => {
    expect(actionsForLoopStatus("  Running ").pause).toBe(true);
    expect(actionsForLoopStatus("PAUSED").unpause).toBe(true);
  });

  it("pause: running, queued, waiting, idle", () => {
    for (const s of ["running", "queued", "waiting", "idle"]) {
      expect(actionsForLoopStatus(s).pause, s).toBe(true);
    }
    expect(actionsForLoopStatus("paused").pause).toBe(false);
    expect(actionsForLoopStatus("failed").pause).toBe(false);
  });

  it("unpause: paused only", () => {
    expect(actionsForLoopStatus("paused").unpause).toBe(true);
    expect(actionsForLoopStatus("running").unpause).toBe(false);
    expect(actionsForLoopStatus("stopped").unpause).toBe(false);
  });

  it("retry: failed, paused, interrupted — not running or stopped", () => {
    for (const s of ["failed", "paused", "interrupted"]) {
      expect(actionsForLoopStatus(s).retry, s).toBe(true);
    }
    expect(actionsForLoopStatus("running").retry).toBe(false);
    expect(actionsForLoopStatus("queued").retry).toBe(false);
    expect(actionsForLoopStatus("stopped").retry).toBe(false);
    expect(actionsForLoopStatus("human_takeover").retry).toBe(false);
  });

  it("stop: hasActiveRun or status running", () => {
    expect(actionsForLoopStatus("running").stop).toBe(true);
    expect(actionsForLoopStatus("waiting").stop).toBe(false);
    expect(actionsForLoopStatus("waiting", { hasActiveRun: true }).stop).toBe(
      true,
    );
    expect(actionsForLoopStatus("paused", { hasActiveRun: true }).stop).toBe(
      true,
    );
    expect(actionsForLoopStatus("failed").stop).toBe(false);
  });

  it("takeover: running, waiting, awaiting_human (not human_takeover)", () => {
    for (const s of ["running", "waiting", "awaiting_human"]) {
      expect(actionsForLoopStatus(s).takeover, s).toBe(true);
    }
    // Spec marked human_takeover uncertain → disable
    expect(actionsForLoopStatus("human_takeover").takeover).toBe(false);
    expect(actionsForLoopStatus("paused").takeover).toBe(false);
    expect(actionsForLoopStatus("failed").takeover).toBe(false);
  });

  it("handback: human_takeover only", () => {
    expect(actionsForLoopStatus("human_takeover").handback).toBe(true);
    expect(actionsForLoopStatus("running").handback).toBe(false);
    expect(actionsForLoopStatus("awaiting_human").handback).toBe(false);
  });

  it("unknown / empty status disables all", () => {
    expect(enabledOf("")).toEqual([]);
    expect(enabledOf("mystery")).toEqual([]);
    expect(enabledOf("completed")).toEqual([]);
    expect(enabledOf("terminated")).toEqual([]);
  });

  it("running enables pause, stop, takeover", () => {
    expect(enabledOf("running").sort()).toEqual(
      ["pause", "stop", "takeover"].sort(),
    );
  });

  it("human_takeover enables handback only", () => {
    expect(enabledOf("human_takeover")).toEqual(["handback"]);
  });

  it("paused enables unpause and retry", () => {
    expect(enabledOf("paused").sort()).toEqual(["retry", "unpause"].sort());
  });
});
