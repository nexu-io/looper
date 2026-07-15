import { describe, expect, it } from "vitest";
import {
  RECONNECT_BACKOFF_MS,
  nextReconnectDelayMs,
  resolveLogsStreamStatus,
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
