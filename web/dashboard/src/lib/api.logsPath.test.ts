import { describe, expect, it } from "vitest";
import { loopLogsFollowPath } from "./api";

describe("loopLogsFollowPath", () => {
  it("opens default follow without stderr flag", () => {
    expect(loopLogsFollowPath("loop_1")).toBe(
      "/api/v1/loops/loop_1/logs?follow=1",
    );
  });

  it("adds stderr=1 when following stderr", () => {
    expect(loopLogsFollowPath("loop_1", { stderr: true })).toBe(
      "/api/v1/loops/loop_1/logs?follow=1&stderr=1",
    );
  });

  it("encodes selector path segments", () => {
    expect(loopLogsFollowPath("a/b")).toBe(
      "/api/v1/loops/a%2Fb/logs?follow=1",
    );
  });
});
