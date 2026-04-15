import { describe, expect, test } from "bun:test";

import {
  LOOPERD_SUPPORTED_TARGETS,
  buildGitHubReleaseApiUrl,
  findLooperdReleaseAssets,
  findReleaseAssetByExactName,
  resolveGitHubReleaseVersion,
  resolveLooperdReleaseAssetNames,
} from "./daemon-release";

describe("daemon release helpers", () => {
  test("returns stable looperd asset names for supported targets", () => {
    expect(LOOPERD_SUPPORTED_TARGETS).toEqual(["darwin-arm64", "darwin-x64"]);

    expect(resolveLooperdReleaseAssetNames("darwin-arm64")).toEqual({
      binary: "looperd-darwin-arm64",
      checksum: "looperd-darwin-arm64.sha256",
    });

    expect(resolveLooperdReleaseAssetNames("darwin-x64")).toEqual({
      binary: "looperd-darwin-x64",
      checksum: "looperd-darwin-x64.sha256",
    });
  });

  test("builds GitHub Releases API URL for latest release", () => {
    expect(
      buildGitHubReleaseApiUrl({ owner: "powerformer", repo: "looper" }),
    ).toBe("https://api.github.com/repos/powerformer/looper/releases/latest");
  });

  test("builds GitHub Releases API URL for a specific tag", () => {
    expect(
      buildGitHubReleaseApiUrl({
        owner: "powerformer",
        repo: "looper",
        tag: "v0.2.0",
      }),
    ).toBe(
      "https://api.github.com/repos/powerformer/looper/releases/tags/v0.2.0",
    );
  });

  test("normalizes release tag names into versions", () => {
    expect(resolveGitHubReleaseVersion({ tag_name: "v0.2.0" })).toBe("0.2.0");
    expect(resolveGitHubReleaseVersion({ tag_name: "0.3.0" })).toBe("0.3.0");
  });

  test("findReleaseAssetByExactName matches exact names only", () => {
    const release = {
      assets: [
        {
          name: "looperd-darwin-arm64",
          browser_download_url: "https://example.invalid/looperd-darwin-arm64",
        },
        {
          name: "looperd-v0.2.0-darwin-arm64",
          browser_download_url:
            "https://example.invalid/looperd-v0.2.0-darwin-arm64",
        },
      ],
    };

    expect(
      findReleaseAssetByExactName(release, "looperd-darwin-arm64")
        ?.browser_download_url,
    ).toBe("https://example.invalid/looperd-darwin-arm64");
    expect(
      findReleaseAssetByExactName(release, "looperd-darwin-arm"),
    ).toBeNull();
  });

  test("finds both binary and checksum assets for a target", () => {
    const release = {
      assets: [
        {
          name: "looperd-darwin-arm64",
          browser_download_url: "https://example.invalid/looperd-darwin-arm64",
        },
        {
          name: "looperd-darwin-arm64.sha256",
          browser_download_url:
            "https://example.invalid/looperd-darwin-arm64.sha256",
        },
        {
          name: "looperd-darwin-x64",
          browser_download_url: "https://example.invalid/looperd-darwin-x64",
        },
      ],
    };

    expect(
      findLooperdReleaseAssets({ release, target: "darwin-arm64" }),
    ).toEqual({
      binary: {
        name: "looperd-darwin-arm64",
        browser_download_url: "https://example.invalid/looperd-darwin-arm64",
      },
      checksum: {
        name: "looperd-darwin-arm64.sha256",
        browser_download_url:
          "https://example.invalid/looperd-darwin-arm64.sha256",
      },
    });
  });

  test("throws when required assets are missing", () => {
    const release = {
      assets: [
        {
          name: "looperd-darwin-x64",
          browser_download_url: "https://example.invalid/looperd-darwin-x64",
        },
      ],
    };

    expect(() =>
      findLooperdReleaseAssets({
        release,
        target: "darwin-arm64",
      }),
    ).toThrow(
      "GitHub release is missing required looperd asset(s): looperd-darwin-arm64, looperd-darwin-arm64.sha256",
    );
  });
});
