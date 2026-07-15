import { describe, expect, it } from "vitest";
import {
  MAX_CHUNK_CHARS,
  MAX_LOG_CHARS,
  MAX_LOG_LINES,
  MAX_SNAPSHOT_SEED_CHARS,
  MAX_SSE_EVENT_DATA_CHARS,
  capFromEnd,
  capLogChunk,
  capLogSeed,
  capSSEEventData,
  trimLogBuffer,
} from "./logBuffer";

describe("logBuffer", () => {
  it("capFromEnd keeps the tail", () => {
    expect(capFromEnd("abcdefghij", 5)).toBe("fghij");
    expect(capFromEnd("short", 100)).toBe("short");
  });

  it("caps seed / chunk / sse data to configured limits", () => {
    const big = "x".repeat(MAX_SNAPSHOT_SEED_CHARS + 50);
    expect(capLogSeed(big).length).toBeLessThanOrEqual(MAX_SNAPSHOT_SEED_CHARS);

    const chunk = "y".repeat(MAX_CHUNK_CHARS + 10);
    expect(capLogChunk(chunk).length).toBeLessThanOrEqual(MAX_CHUNK_CHARS);

    const sse = "z".repeat(MAX_SSE_EVENT_DATA_CHARS + 10);
    expect(capSSEEventData(sse).length).toBeLessThanOrEqual(
      MAX_SSE_EVENT_DATA_CHARS,
    );
  });

  it("trimLogBuffer enforces char and line caps", () => {
    const byChars = "a".repeat(MAX_LOG_CHARS + 100);
    expect(trimLogBuffer(byChars).length).toBeLessThanOrEqual(MAX_LOG_CHARS);

    const byLines = Array.from({ length: MAX_LOG_LINES + 20 }, (_, i) =>
      `line-${i}`,
    ).join("\n");
    expect(trimLogBuffer(byLines).split("\n").length).toBeLessThanOrEqual(
      MAX_LOG_LINES,
    );
  });
});
