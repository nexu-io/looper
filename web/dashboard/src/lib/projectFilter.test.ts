import { describe, expect, it } from "vitest";
import { resolveValidatedProjectFilter } from "./projectFilter";

describe("resolveValidatedProjectFilter", () => {
  it("returns empty when stored id is empty", () => {
    expect(resolveValidatedProjectFilter("", [{ id: "p1" }])).toBe("");
  });

  it("treats loading/error (null items) as All", () => {
    expect(resolveValidatedProjectFilter("p1", null)).toBe("");
  });

  it("rejects ids not present in the loaded list", () => {
    expect(
      resolveValidatedProjectFilter("missing", [{ id: "p1" }, { id: "p2" }]),
    ).toBe("");
  });

  it("accepts a validated id after projects load", () => {
    expect(
      resolveValidatedProjectFilter("p2", [{ id: "p1" }, { id: "p2" }]),
    ).toBe("p2");
  });
});
