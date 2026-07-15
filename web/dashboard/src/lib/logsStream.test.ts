import { describe, expect, it } from "vitest";
import {
  RECONNECT_BACKOFF_MS,
  formatLiveStderrChunk,
  needsSeparateStderrFollow,
  nextReconnectDelayMs,
  resolveLogsStreamStatus,
  stderrGapFromSecondarySnapshot,
} from "./logsStream";

describe("resolveLogsStreamStatus", () => {
  it("shows connecting while reconnecting even if prior logs are retained", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "connecting",
        ended: false,
        error: null,
      }),
    ).toBe("connecting");
  });

  it("shows live only after phase advances past connecting", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "live",
        ended: false,
        error: null,
      }),
    ).toBe("live");
  });

  it("prefers error and ended over phase", () => {
    expect(
      resolveLogsStreamStatus({
        phase: "connecting",
        ended: false,
        error: "boom",
      }),
    ).toBe("error");
    expect(
      resolveLogsStreamStatus({
        phase: "live",
        ended: true,
        error: null,
      }),
    ).toBe("ended");
  });
});

describe("nextReconnectDelayMs", () => {
  it("uses bounded backoff 1s, 2s, 5s max", () => {
    expect(nextReconnectDelayMs(0)).toBe(1000);
    expect(nextReconnectDelayMs(1)).toBe(2000);
    expect(nextReconnectDelayMs(2)).toBe(5000);
    expect(nextReconnectDelayMs(10)).toBe(5000);
    expect(RECONNECT_BACKOFF_MS).toEqual([1000, 2000, 5000]);
  });
});

describe("needsSeparateStderrFollow", () => {
  it("is true when default follow would stick to non-empty stdout", () => {
    expect(
      needsSeparateStderrFollow({ stdout: "out\n", stderr: "err\n" }),
    ).toBe(true);
    expect(needsSeparateStderrFollow({ stdout: "out\n", stderr: "" })).toBe(
      true,
    );
  });

  it("is false when default follow already covers stderr-only output", () => {
    expect(needsSeparateStderrFollow({ stdout: "", stderr: "err\n" })).toBe(
      false,
    );
    expect(needsSeparateStderrFollow({ stdout: "  ", stderr: "err\n" })).toBe(
      false,
    );
    expect(needsSeparateStderrFollow(null)).toBe(false);
    expect(needsSeparateStderrFollow(undefined)).toBe(false);
  });
});

describe("formatLiveStderrChunk", () => {
  it("adds a stderr section header only for the first live chunk", () => {
    expect(formatLiveStderrChunk("boom\n", false)).toBe(
      "\n--- stderr ---\nboom\n",
    );
    expect(formatLiveStderrChunk("more\n", true)).toBe("more\n");
    expect(formatLiveStderrChunk("", false)).toBe("");
  });
});

describe("stderrGapFromSecondarySnapshot", () => {
  it("returns empty when secondary has no new stderr", () => {
    expect(stderrGapFromSecondarySnapshot("err\n", "err\n")).toBe("");
    expect(stderrGapFromSecondarySnapshot("err\n", "")).toBe("");
    expect(stderrGapFromSecondarySnapshot("", "")).toBe("");
  });

  it("returns full secondary when primary had none", () => {
    expect(stderrGapFromSecondarySnapshot("", "late\n")).toBe("late\n");
  });

  it("returns only the append suffix when secondary extends primary", () => {
    expect(stderrGapFromSecondarySnapshot("a\n", "a\nb\n")).toBe("b\n");
  });

  it("returns full secondary on non-prefix rewrite", () => {
    expect(stderrGapFromSecondarySnapshot("old\n", "new\n")).toBe("new\n");
  });
});
