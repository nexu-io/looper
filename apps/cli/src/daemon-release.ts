export const LOOPERD_SUPPORTED_TARGETS = [
  "darwin-arm64",
  "darwin-x64",
] as const;

export type LooperdTarget = (typeof LOOPERD_SUPPORTED_TARGETS)[number];

export interface LooperdReleaseAssetNames {
  binary: string;
  checksum: string;
}

export interface GitHubReleaseAsset {
  name: string;
  browser_download_url: string;
}

export interface GitHubReleasePayload {
  tag_name?: string;
  assets: GitHubReleaseAsset[];
}

export function resolveLooperdReleaseAssetNames(
  target: LooperdTarget,
): LooperdReleaseAssetNames {
  const binary = `looperd-${target}`;
  return {
    binary,
    checksum: `${binary}.sha256`,
  };
}

export function buildGitHubReleaseApiUrl(options: {
  owner: string;
  repo: string;
  tag?: string;
}): string {
  const { owner, repo, tag } = options;
  const base = `https://api.github.com/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases`;

  if (tag && tag.length > 0) {
    return `${base}/tags/${encodeURIComponent(tag)}`;
  }

  return `${base}/latest`;
}

export function findReleaseAssetByExactName(
  release: GitHubReleasePayload,
  assetName: string,
): GitHubReleaseAsset | null {
  return release.assets.find((asset) => asset.name === assetName) ?? null;
}

export function findLooperdReleaseAssets(options: {
  release: GitHubReleasePayload;
  target: LooperdTarget;
}): { binary: GitHubReleaseAsset; checksum: GitHubReleaseAsset } {
  const names = resolveLooperdReleaseAssetNames(options.target);
  const binary = findReleaseAssetByExactName(options.release, names.binary);
  const checksum = findReleaseAssetByExactName(options.release, names.checksum);

  if (binary === null || checksum === null) {
    const missing: string[] = [];
    if (binary === null) {
      missing.push(names.binary);
    }
    if (checksum === null) {
      missing.push(names.checksum);
    }

    throw new Error(
      `GitHub release is missing required looperd asset(s): ${missing.join(", ")}`,
    );
  }

  return { binary, checksum };
}

export function resolveGitHubReleaseVersion(
  release: Pick<GitHubReleasePayload, "tag_name">,
): string {
  const raw = release.tag_name?.trim();
  if (!raw) {
    throw new Error("GitHub release payload is missing tag_name");
  }

  return raw.startsWith("v") ? raw.slice(1) : raw;
}
