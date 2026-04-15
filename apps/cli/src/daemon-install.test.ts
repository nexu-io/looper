import { describe, expect, test } from "bun:test";

import type { FetchLike } from "./client";
import {
  installLooperdBinary,
  resolveLooperdTarget,
  type DaemonInstallFs,
} from "./daemon-install";

describe("daemon install helpers", () => {
  test("detects supported looperd targets from platform and arch", () => {
    expect(resolveLooperdTarget("darwin", "arm64")).toBe("darwin-arm64");
    expect(resolveLooperdTarget("darwin", "x64")).toBe("darwin-x64");
  });

  test("throws for unsupported platform and arch", () => {
    expect(() => resolveLooperdTarget("linux", "x64")).toThrow(
      "Unsupported platform/arch for looperd install: linux-x64. Supported targets: darwin-arm64, darwin-x64",
    );
  });

  test("fetches release metadata, selects exact asset and installs binary", async () => {
    const writes: Array<{ path: string; content: Uint8Array }> = [];
    const chmods: Array<{ path: string; mode: number }> = [];
    const fetchCalls: string[] = [];

    const fs: Partial<DaemonInstallFs> = {
      statImpl: async () => {
        throw new Error("missing");
      },
      mkdirImpl: async () => undefined,
      writeFileImpl: async (path, content) => {
        writes.push({ path: String(path), content: content as Uint8Array });
      },
      chmodImpl: async (path, mode) => {
        chmods.push({ path: String(path), mode: Number(mode) });
      },
    };

    const fetchImpl: FetchLike = async (input) => {
      const url = String(input);
      fetchCalls.push(url);

      if (url.includes("/releases/latest")) {
        return new Response(
          JSON.stringify({
            assets: [
              {
                name: "looperd-darwin-arm64",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-arm64",
              },
              {
                name: "looperd-darwin-arm64.sha256",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-arm64.sha256",
              },
            ],
          }),
        );
      }

      if (url === "https://example.invalid/looperd-darwin-arm64") {
        return new Response(new Uint8Array([1, 2, 3]));
      }

      throw new Error(`unexpected url ${url}`);
    };

    const result = await installLooperdBinary({
      fetchImpl,
      platform: "darwin",
      arch: "arm64",
      homeDir: "/Users/tester",
      force: false,
      fs,
    });

    expect(result).toEqual({
      target: "darwin-arm64",
      installPath: "/Users/tester/.looper/bin/looperd",
      downloadedFrom: "https://example.invalid/looperd-darwin-arm64",
      skipped: false,
    });
    expect(fetchCalls).toEqual([
      "https://api.github.com/repos/powerformer/looper/releases/latest",
      "https://example.invalid/looperd-darwin-arm64",
    ]);
    expect(writes).toHaveLength(1);
    expect(writes[0]?.path).toBe("/Users/tester/.looper/bin/looperd");
    expect(Array.from(writes[0]?.content ?? [])).toEqual([1, 2, 3]);
    expect(chmods).toEqual([
      {
        path: "/Users/tester/.looper/bin/looperd",
        mode: 0o755,
      },
    ]);
  });

  test("is idempotent by default when looperd is already installed", async () => {
    const fetchImpl: FetchLike = async () => {
      throw new Error("fetch should not be called");
    };

    const result = await installLooperdBinary({
      fetchImpl,
      platform: "darwin",
      arch: "x64",
      homeDir: "/Users/tester",
      force: false,
      fs: {
        statImpl: async () => ({}) as never,
      },
    });

    expect(result).toEqual({
      target: "darwin-x64",
      installPath: "/Users/tester/.looper/bin/looperd",
      downloadedFrom: null,
      skipped: true,
    });
  });

  test("overwrites existing install when force is enabled", async () => {
    let downloadCount = 0;

    const fetchImpl: FetchLike = async (input) => {
      const url = String(input);
      if (url.includes("/releases/latest")) {
        return new Response(
          JSON.stringify({
            assets: [
              {
                name: "looperd-darwin-x64",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-x64",
              },
              {
                name: "looperd-darwin-x64.sha256",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-x64.sha256",
              },
            ],
          }),
        );
      }

      if (url === "https://example.invalid/looperd-darwin-x64") {
        downloadCount += 1;
        return new Response(new Uint8Array([7]));
      }

      throw new Error(`unexpected url ${url}`);
    };

    const writes: string[] = [];
    await installLooperdBinary({
      fetchImpl,
      platform: "darwin",
      arch: "x64",
      homeDir: "/Users/tester",
      force: true,
      fs: {
        statImpl: async () => ({}) as never,
        mkdirImpl: async () => undefined,
        writeFileImpl: async (path) => {
          writes.push(String(path));
        },
        chmodImpl: async () => undefined,
      },
    });

    expect(downloadCount).toBe(1);
    expect(writes).toEqual(["/Users/tester/.looper/bin/looperd"]);
  });

  test("reports clear error when metadata request fails", async () => {
    const fetchImpl: FetchLike = async () =>
      new Response("nope", {
        status: 503,
        statusText: "Service Unavailable",
      });

    await expect(() =>
      installLooperdBinary({
        fetchImpl,
        platform: "darwin",
        arch: "arm64",
        homeDir: "/Users/tester",
        force: false,
        fs: {
          statImpl: async () => {
            throw new Error("missing");
          },
        },
      }),
    ).toThrow(
      "Failed to fetch GitHub release metadata from https://api.github.com/repos/powerformer/looper/releases/latest (status 503 Service Unavailable)",
    );
  });

  test("reports clear error when binary asset download fails", async () => {
    const fetchImpl: FetchLike = async (input) => {
      const url = String(input);
      if (url.includes("/releases/latest")) {
        return new Response(
          JSON.stringify({
            assets: [
              {
                name: "looperd-darwin-arm64",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-arm64",
              },
              {
                name: "looperd-darwin-arm64.sha256",
                browser_download_url:
                  "https://example.invalid/looperd-darwin-arm64.sha256",
              },
            ],
          }),
        );
      }

      return new Response("not found", {
        status: 404,
        statusText: "Not Found",
      });
    };

    await expect(() =>
      installLooperdBinary({
        fetchImpl,
        platform: "darwin",
        arch: "arm64",
        homeDir: "/Users/tester",
        force: false,
        fs: {
          statImpl: async () => {
            throw new Error("missing");
          },
        },
      }),
    ).toThrow(
      "Failed to download looperd binary from https://example.invalid/looperd-darwin-arm64 (status 404 Not Found)",
    );
  });
});
