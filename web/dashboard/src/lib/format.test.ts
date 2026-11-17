import { describe, expect, it } from "vitest";
import {
  formatAge,
  formatAttempts,
  issueUrl,
  parseIssueTargetId,
  pullRequestUrl,
  repositoryUrl,
  truncateReason,
} from "./format";

describe("formatAge", () => {
  const now = Date.parse("2026-04-11T12:00:00.000Z");

  it("formats compact elapsed durations", () => {
    expect(formatAge("2026-04-11T11:59:48.000Z", now)).toBe("12s");
    expect(formatAge("2026-04-11T11:57:00.000Z", now)).toBe("3m");
    expect(formatAge("2026-04-11T10:00:00.000Z", now)).toBe("2h");
    expect(formatAge("2026-04-11T09:40:00.000Z", now)).toBe("2h 20m");
    expect(formatAge("2026-04-09T12:00:00.000Z", now)).toBe("2d");
    expect(formatAge("2026-04-09T08:00:00.000Z", now)).toBe("2d 4h");
  });

  it("returns em dash for missing or invalid input", () => {
    expect(formatAge(null, now)).toBe("—");
    expect(formatAge(undefined, now)).toBe("—");
    expect(formatAge("not-a-date", now)).toBe("—");
  });
});

describe("formatAttempts", () => {
  it("formats current/max including unlimited as infinity", () => {
    expect(formatAttempts(2, 5)).toBe("2/5");
    expect(formatAttempts(1, -1)).toBe("1/∞");
    expect(formatAttempts(0, 3)).toBe("0/3");
  });

  it("returns current only when max is missing", () => {
    expect(formatAttempts(2, null)).toBe("2");
    expect(formatAttempts(2, undefined)).toBe("2");
  });

  it("returns null when attempts metadata is absent", () => {
    expect(formatAttempts(null, 3)).toBeNull();
    expect(formatAttempts(undefined, -1)).toBeNull();
    expect(formatAttempts(Number.NaN, 3)).toBeNull();
  });
});

describe("repositoryUrl", () => {
  it("prefers server-resolved repoUrl", () => {
    expect(
      repositoryUrl("acme/fj", "https://code.example.com/acme/fj"),
    ).toBe("https://code.example.com/acme/fj");
  });

  it("builds github repo urls from owner/repo as fallback", () => {
    expect(repositoryUrl("powerformer/vela")).toBe(
      "https://github.com/powerformer/vela",
    );
  });

  it("returns null for incomplete or invalid inputs", () => {
    expect(repositoryUrl(null)).toBeNull();
    expect(repositoryUrl("")).toBeNull();
    expect(repositoryUrl("bare")).toBeNull();
    expect(repositoryUrl("https://github.com/o/r")).toBeNull();
  });
});

describe("pullRequestUrl", () => {
  it("builds github PR urls from owner/repo + number", () => {
    expect(pullRequestUrl("powerformer/vela", 1217)).toBe(
      "https://github.com/powerformer/vela/pull/1217",
    );
  });

  it("uses forgejo /pulls path when provider is forgejo", () => {
    expect(
      pullRequestUrl("acme/fj", 42, {
        repoUrl: "https://code.example.com/acme/fj",
        provider: "forgejo",
      }),
    ).toBe("https://code.example.com/acme/fj/pulls/42");
  });

  it("uses /pull path for github repoUrl", () => {
    expect(
      pullRequestUrl("o/r", 7, {
        repoUrl: "https://github.com/o/r/",
        provider: "github",
      }),
    ).toBe("https://github.com/o/r/pull/7");
  });

  it("returns null for incomplete or invalid inputs", () => {
    expect(pullRequestUrl(null, 1)).toBeNull();
    expect(pullRequestUrl("owner/repo", null)).toBeNull();
    expect(pullRequestUrl("owner/repo", 0)).toBeNull();
    expect(pullRequestUrl("bare", 1)).toBeNull();
    expect(pullRequestUrl("https://github.com/o/r", 1)).toBeNull();
  });
});

describe("issueUrl", () => {
  it("builds github issue urls from owner/repo + number", () => {
    expect(issueUrl("powerformer/vela", 77)).toBe(
      "https://github.com/powerformer/vela/issues/77",
    );
  });

  it("uses server repoUrl when provided (forgejo self-host)", () => {
    expect(
      issueUrl("acme/fj", 42, {
        repoUrl: "https://code.example.com/acme/fj/",
      }),
    ).toBe("https://code.example.com/acme/fj/issues/42");
  });

  it("returns null for incomplete or invalid inputs", () => {
    expect(issueUrl(null, 1)).toBeNull();
    expect(issueUrl("owner/repo", null)).toBeNull();
    expect(issueUrl("owner/repo", 0)).toBeNull();
    expect(issueUrl("bare", 1)).toBeNull();
  });
});

describe("parseIssueTargetId", () => {
  it("parses canonical issue target key", () => {
    expect(parseIssueTargetId("issue:acme/looper:77")).toEqual({
      repo: "acme/looper",
      issueNumber: 77,
    });
  });

  it("rejects non-issue or malformed keys", () => {
    expect(parseIssueTargetId(null)).toBeNull();
    expect(parseIssueTargetId("")).toBeNull();
    expect(parseIssueTargetId("project:acme")).toBeNull();
    expect(parseIssueTargetId("issue:acme/looper")).toBeNull();
    expect(parseIssueTargetId("issue:barerepo:1")).toBeNull();
    expect(parseIssueTargetId("issue:acme/looper:abc")).toBeNull();
  });
});

describe("truncateReason", () => {
  it("returns null for empty/missing", () => {
    expect(truncateReason(null)).toBeNull();
    expect(truncateReason(undefined)).toBeNull();
    expect(truncateReason("")).toBeNull();
    expect(truncateReason("   \n\t  ")).toBeNull();
  });

  it("collapses whitespace and truncates with ellipsis", () => {
    expect(truncateReason("agent idle\n timed  out", 64)).toBe(
      "agent idle timed out",
    );
    const long = "x".repeat(80);
    expect(truncateReason(long, 20)).toBe(`${"x".repeat(17)}...`);
  });

  it("does not truncate short text", () => {
    expect(truncateReason("dirty worktree", 64)).toBe("dirty worktree");
  });
});
