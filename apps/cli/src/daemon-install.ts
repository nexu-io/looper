import { chmod, mkdir, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";

import {
  type GitHubReleasePayload,
  type LooperdTarget,
  buildGitHubReleaseApiUrl,
  findLooperdReleaseAssets,
} from "./daemon-release";
import type { FetchLike } from "./client";

export interface DaemonInstallResult {
  target: LooperdTarget;
  installPath: string;
  downloadedFrom: string | null;
  skipped: boolean;
}

export interface DaemonInstallFs {
  mkdirImpl: typeof mkdir;
  writeFileImpl: typeof writeFile;
  chmodImpl: typeof chmod;
  statImpl: typeof stat;
}

export async function installLooperdBinary(options: {
  fetchImpl: FetchLike;
  platform: NodeJS.Platform;
  arch: string;
  homeDir: string;
  force: boolean;
  owner?: string;
  repo?: string;
  tag?: string;
  fs?: Partial<DaemonInstallFs>;
}): Promise<DaemonInstallResult> {
  const target = resolveLooperdTarget(options.platform, options.arch);
  const installDir = join(options.homeDir, ".looper", "bin");
  const installPath = join(installDir, "looperd");

  const mkdirImpl = options.fs?.mkdirImpl ?? mkdir;
  const writeFileImpl = options.fs?.writeFileImpl ?? writeFile;
  const chmodImpl = options.fs?.chmodImpl ?? chmod;
  const statImpl = options.fs?.statImpl ?? stat;

  if (!options.force) {
    const exists = await fileExists(statImpl, installPath);
    if (exists) {
      return {
        target,
        installPath,
        downloadedFrom: null,
        skipped: true,
      };
    }
  }

  const owner = options.owner ?? "powerformer";
  const repo = options.repo ?? "looper";
  const releaseUrl = buildGitHubReleaseApiUrl({
    owner,
    repo,
    tag: options.tag,
  });
  const releasePayload = await fetchReleaseMetadata(
    options.fetchImpl,
    releaseUrl,
  );
  const { binary } = findLooperdReleaseAssets({
    release: releasePayload,
    target,
  });

  const binaryResponse = await options.fetchImpl(binary.browser_download_url, {
    headers: {
      "user-agent": "looper-cli",
      accept: "application/octet-stream",
    },
  });
  if (!binaryResponse.ok) {
    throw new Error(
      `Failed to download looperd binary from ${binary.browser_download_url} (status ${binaryResponse.status} ${binaryResponse.statusText})`,
    );
  }

  const binaryBytes = new Uint8Array(await binaryResponse.arrayBuffer());
  await mkdirImpl(installDir, { recursive: true });
  await writeFileImpl(installPath, binaryBytes);
  await chmodImpl(installPath, 0o755);

  return {
    target,
    installPath,
    downloadedFrom: binary.browser_download_url,
    skipped: false,
  };
}

export function resolveLooperdTarget(
  platform: NodeJS.Platform,
  arch: string,
): LooperdTarget {
  if (platform === "darwin" && arch === "arm64") {
    return "darwin-arm64";
  }

  if (platform === "darwin" && arch === "x64") {
    return "darwin-x64";
  }

  throw new Error(
    `Unsupported platform/arch for looperd install: ${platform}-${arch}. Supported targets: darwin-arm64, darwin-x64`,
  );
}

async function fetchReleaseMetadata(
  fetchImpl: FetchLike,
  releaseUrl: string,
): Promise<GitHubReleasePayload> {
  const response = await fetchImpl(releaseUrl, {
    headers: {
      "user-agent": "looper-cli",
      accept: "application/vnd.github+json",
    },
  });

  if (!response.ok) {
    throw new Error(
      `Failed to fetch GitHub release metadata from ${releaseUrl} (status ${response.status} ${response.statusText})`,
    );
  }

  const payload = (await response.json()) as Partial<GitHubReleasePayload>;
  if (!Array.isArray(payload.assets)) {
    throw new Error(
      `GitHub release payload is missing assets array: ${releaseUrl}`,
    );
  }

  return {
    assets: payload.assets,
  };
}

async function fileExists(
  statImpl: typeof stat,
  filePath: string,
): Promise<boolean> {
  try {
    await statImpl(filePath);
    return true;
  } catch {
    return false;
  }
}
