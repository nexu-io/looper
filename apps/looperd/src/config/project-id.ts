import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { normalize, posix, resolve, win32 } from "node:path";

const PROJECT_ID_SEPARATOR_PATTERN = /[\\/]/;
const LEGACY_PROJECT_ID_PREFIX = "legacy-id-";
const AUTO_DERIVED_LEGACY_PROJECT_ID_PREFIX = "project_";
const CANONICAL_PROJECT_DIRECTORY_NAME_PATTERN = /^[a-z0-9._-]+$/;
const MAX_PROJECT_WORKTREE_DIRECTORY_NAME_LENGTH = 255;
const WINDOWS_RESERVED_PROJECT_DIRECTORY_BASENAME_PATTERN =
  /^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$/;
const INVALID_PROJECT_ID_MESSAGE =
  "must not contain path separators, dot segments, be an absolute path, or start with legacy-id-";
const CONFIG_PROJECT_ID_MESSAGE =
  "must not contain path separators, dot segments, or be an absolute path";

function toHashedProjectWorktreeDirectoryName(projectId: string): string {
  const hashedProjectId = createHash("sha256").update(projectId).digest("hex");

  return `${LEGACY_PROJECT_ID_PREFIX}${hashedProjectId}`;
}

export function toRepoWorktreeDirectoryName(repoIdentity: string): string {
  return `repo-${createHash("sha256").update(canonicalizeRepoIdentity(repoIdentity)).digest("hex")}`;
}

export function normalizeDerivedProjectId(projectId: string): string {
  if (!projectId.startsWith(LEGACY_PROJECT_ID_PREFIX)) {
    return projectId;
  }

  return `${AUTO_DERIVED_LEGACY_PROJECT_ID_PREFIX}${projectId}`;
}

export function deriveProjectIdFromRepoPath(repoPath: string): string {
  const segments = repoPath.split(/[\\/]+/).filter(Boolean);
  const lastSegment = segments.at(-1) ?? "project";
  const normalized = lastSegment
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return normalized || "project";
}

export class InvalidProjectIdError extends Error {
  constructor(projectId: string) {
    super(`Invalid project id \"${projectId}\": ${INVALID_PROJECT_ID_MESSAGE}`);
    this.name = "InvalidProjectIdError";
  }
}

export function isValidProjectId(projectId: string): boolean {
  return (
    projectId.length > 0 &&
    projectId !== "." &&
    projectId !== ".." &&
    !projectId.startsWith(LEGACY_PROJECT_ID_PREFIX) &&
    !PROJECT_ID_SEPARATOR_PATTERN.test(projectId) &&
    !posix.isAbsolute(projectId) &&
    !win32.isAbsolute(projectId)
  );
}

export function getProjectIdValidationMessage(): string {
  return INVALID_PROJECT_ID_MESSAGE;
}

export function getConfigProjectIdValidationMessage(): string {
  return CONFIG_PROJECT_ID_MESSAGE;
}

export function assertValidProjectId(projectId: string): void {
  if (!isValidProjectId(projectId)) {
    throw new InvalidProjectIdError(projectId);
  }
}

export function isValidConfiguredProjectId(projectId: string): boolean {
  return (
    projectId.length > 0 &&
    projectId !== "." &&
    projectId !== ".." &&
    !PROJECT_ID_SEPARATOR_PATTERN.test(projectId) &&
    !posix.isAbsolute(projectId) &&
    !win32.isAbsolute(projectId)
  );
}

function canonicalizeRepoIdentity(repoIdentity: string): string {
  const resolved = normalize(resolve(repoIdentity));

  try {
    return normalize(realpathSync.native(resolved));
  } catch {
    return resolved;
  }
}

function isWindowsInvalidCanonicalProjectDirectoryName(
  directoryName: string,
): boolean {
  return (
    directoryName.endsWith(".") ||
    WINDOWS_RESERVED_PROJECT_DIRECTORY_BASENAME_PATTERN.test(directoryName)
  );
}

export function toProjectWorktreeDirectoryName(projectId: string): string {
  if (
    isValidProjectId(projectId) &&
    CANONICAL_PROJECT_DIRECTORY_NAME_PATTERN.test(projectId) &&
    projectId.length <= MAX_PROJECT_WORKTREE_DIRECTORY_NAME_LENGTH &&
    !isWindowsInvalidCanonicalProjectDirectoryName(projectId)
  ) {
    return projectId;
  }

  const encodedProjectId = Buffer.from(projectId).toString("hex") || "empty";

  if (
    LEGACY_PROJECT_ID_PREFIX.length + encodedProjectId.length <=
    MAX_PROJECT_WORKTREE_DIRECTORY_NAME_LENGTH
  ) {
    return `${LEGACY_PROJECT_ID_PREFIX}${encodedProjectId}`;
  }

  return toHashedProjectWorktreeDirectoryName(projectId);
}
