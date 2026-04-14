import { posix, win32 } from "node:path";

const PROJECT_ID_SEPARATOR_PATTERN = /[\\/]/;
const INVALID_PROJECT_ID_MESSAGE =
  "must not contain path separators, dot segments, or be an absolute path";

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
