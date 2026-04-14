import { posix, win32 } from "node:path";

const PROJECT_ID_SEPARATOR_PATTERN = /[\\/]/;
const LEGACY_PROJECT_ID_PREFIX = "legacy-id-";
const CANONICAL_PROJECT_DIRECTORY_NAME_PATTERN = /^[a-z0-9._-]+$/;
const INVALID_PROJECT_ID_MESSAGE =
  "must not contain path separators, dot segments, be an absolute path, or start with legacy-id-";

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

export function assertValidProjectId(projectId: string): void {
  if (!isValidProjectId(projectId)) {
    throw new InvalidProjectIdError(projectId);
  }
}

export function toProjectWorktreeDirectoryName(projectId: string): string {
  if (
    isValidProjectId(projectId) &&
    CANONICAL_PROJECT_DIRECTORY_NAME_PATTERN.test(projectId)
  ) {
    return projectId;
  }

  const encodedProjectId = Buffer.from(projectId).toString("hex") || "empty";

  return `${LEGACY_PROJECT_ID_PREFIX}${encodedProjectId}`;
}
