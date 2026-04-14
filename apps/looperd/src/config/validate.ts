import { constants, access, stat } from "node:fs/promises";
import { dirname } from "node:path";

import { getDefaultWorktreeRoot } from "./defaults";
import {
  getConfigProjectIdValidationMessage,
  isValidConfiguredProjectId,
} from "./project-id";
import {
  AGENT_VENDORS,
  AUTH_MODES,
  ConfigValidationError,
  DAEMON_MODES,
  LOG_LEVELS,
  type LooperConfig,
  NOTIFICATION_SOUND_LEVELS,
  OPEN_PR_STRATEGIES,
  type ValidationIssue,
} from "./types";

function isNonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

function isPositiveInteger(value: unknown): value is number {
  return Number.isInteger(value) && typeof value === "number" && value > 0;
}

function isBoolean(value: unknown): value is boolean {
  return typeof value === "boolean";
}

async function ensureWritablePath(
  path: string,
  kind: "directory" | "file-parent",
  issues: ValidationIssue[],
  field: string,
): Promise<void> {
  const target = kind === "directory" ? path : dirname(path);
  let writableAnchor = target;

  while (true) {
    try {
      const anchorStat = await stat(writableAnchor);
      if (!anchorStat.isDirectory()) {
        issues.push({
          path: field,
          message: `${writableAnchor} is not a directory`,
        });
        return;
      }
      break;
    } catch {
      const parent = dirname(writableAnchor);
      if (parent === writableAnchor) {
        issues.push({
          path: field,
          message: `${target} cannot be created because no existing parent was found`,
        });
        return;
      }

      writableAnchor = parent;
    }
  }

  try {
    await access(writableAnchor, constants.W_OK);
  } catch {
    issues.push({
      path: field,
      message: `${writableAnchor} is not writable`,
    });
  }
}

export async function validateLooperConfig(
  config: LooperConfig,
  options: { defaultWorktreeRoot?: string } = {},
): Promise<void> {
  const issues: ValidationIssue[] = [];

  if (!isNonEmptyString(config.server.host)) {
    issues.push({ path: "server.host", message: "must be a non-empty string" });
  }

  if (!isPositiveInteger(config.server.port) || config.server.port > 65_535) {
    issues.push({
      path: "server.port",
      message: "must be an integer between 1 and 65535",
    });
  }

  if (!AUTH_MODES.includes(config.server.authMode)) {
    issues.push({
      path: "server.authMode",
      message: `must be one of: ${AUTH_MODES.join(", ")}`,
    });
  }

  if (
    config.server.authMode === "local-token" &&
    !isNonEmptyString(config.server.localToken)
  ) {
    issues.push({
      path: "server.localToken",
      message: "is required when authMode is local-token",
    });
  }

  if (config.storage.mode !== "sqlite") {
    issues.push({ path: "storage.mode", message: "must be sqlite" });
  }

  if (!isNonEmptyString(config.storage.dbPath)) {
    issues.push({
      path: "storage.dbPath",
      message: "must be a non-empty path",
    });
  }

  if (
    !isPositiveInteger(config.scheduler.pollIntervalSeconds) ||
    config.scheduler.pollIntervalSeconds < 10
  ) {
    issues.push({
      path: "scheduler.pollIntervalSeconds",
      message: "must be an integer >= 10",
    });
  }

  if (!isPositiveInteger(config.scheduler.maxConcurrentRuns)) {
    issues.push({
      path: "scheduler.maxConcurrentRuns",
      message: "must be a positive integer",
    });
  }

  if (!isPositiveInteger(config.scheduler.retryMaxAttempts)) {
    issues.push({
      path: "scheduler.retryMaxAttempts",
      message: "must be a positive integer",
    });
  }

  if (!isPositiveInteger(config.scheduler.retryBaseDelayMs)) {
    issues.push({
      path: "scheduler.retryBaseDelayMs",
      message: "must be a positive integer",
    });
  }

  if (config.agent.vendor && !AGENT_VENDORS.includes(config.agent.vendor)) {
    issues.push({
      path: "agent.vendor",
      message: `must be one of: ${AGENT_VENDORS.join(", ")}`,
    });
  }

  if (!LOG_LEVELS.includes(config.logging.level)) {
    issues.push({
      path: "logging.level",
      message: `must be one of: ${LOG_LEVELS.join(", ")}`,
    });
  }

  if (!isPositiveInteger(config.logging.maxSizeMB)) {
    issues.push({
      path: "logging.maxSizeMB",
      message: "must be a positive integer",
    });
  }

  if (!isPositiveInteger(config.logging.maxFiles)) {
    issues.push({
      path: "logging.maxFiles",
      message: "must be a positive integer",
    });
  }

  if (
    !isPositiveInteger(config.notifications.osascript.throttleWindowSeconds)
  ) {
    issues.push({
      path: "notifications.osascript.throttleWindowSeconds",
      message: "must be a positive integer",
    });
  }

  for (const level of config.notifications.osascript.soundForLevels ?? []) {
    if (!NOTIFICATION_SOUND_LEVELS.includes(level)) {
      issues.push({
        path: "notifications.osascript.soundForLevels",
        message: `contains unsupported value: ${level}`,
      });
    }
  }

  if (!DAEMON_MODES.includes(config.daemon.mode)) {
    issues.push({
      path: "daemon.mode",
      message: `must be one of: ${DAEMON_MODES.join(", ")}`,
    });
  }

  if (!isNonEmptyString(config.daemon.logDir)) {
    issues.push({ path: "daemon.logDir", message: "must be a non-empty path" });
  }

  if (!isNonEmptyString(config.daemon.workingDirectory)) {
    issues.push({
      path: "daemon.workingDirectory",
      message: "must be a non-empty path",
    });
  }

  if (!isNonEmptyString(config.defaults.baseBranch)) {
    issues.push({
      path: "defaults.baseBranch",
      message: "must be a non-empty string",
    });
  }

  if (!isBoolean(config.defaults.allowAutoCommit)) {
    issues.push({
      path: "defaults.allowAutoCommit",
      message: "must be a boolean",
    });
  }

  if (!isBoolean(config.defaults.allowAutoPush)) {
    issues.push({
      path: "defaults.allowAutoPush",
      message: "must be a boolean",
    });
  }

  if (!isBoolean(config.defaults.allowAutoApprove)) {
    issues.push({
      path: "defaults.allowAutoApprove",
      message: "must be a boolean",
    });
  }

  if (!isBoolean(config.defaults.allowAutoMerge)) {
    issues.push({
      path: "defaults.allowAutoMerge",
      message: "must be a boolean",
    });
  }

  if (!isBoolean(config.defaults.allowRiskyFixes)) {
    issues.push({
      path: "defaults.allowRiskyFixes",
      message: "must be a boolean",
    });
  }

  if (
    config.defaults.openPrStrategy &&
    !OPEN_PR_STRATEGIES.includes(config.defaults.openPrStrategy)
  ) {
    issues.push({
      path: "defaults.openPrStrategy",
      message: `must be one of: ${OPEN_PR_STRATEGIES.join(", ")}`,
    });
  }

  if (!config.tools.bunPath) {
    issues.push({ path: "tools.bunPath", message: "could not be resolved" });
  }

  if (!config.tools.gitPath) {
    issues.push({ path: "tools.gitPath", message: "could not be resolved" });
  }

  if (!config.tools.ghPath) {
    issues.push({ path: "tools.ghPath", message: "could not be resolved" });
  }

  if (config.notifications.osascript.enabled && !config.tools.osascriptPath) {
    issues.push({
      path: "tools.osascriptPath",
      message: "is required when osascript notifications are enabled",
    });
  }

  if (config.daemon.mode === "launchd" && !config.tools.bunPath) {
    issues.push({
      path: "daemon.mode",
      message: "launchd mode requires bunPath to be resolved",
    });
  }

  const projectIds = new Set<string>();
  for (const [index, project] of config.projects.entries()) {
    const prefix = `projects[${index}]`;

    if (!isNonEmptyString(project.id)) {
      issues.push({
        path: `${prefix}.id`,
        message: "must be a non-empty string",
      });
    } else if (!isValidConfiguredProjectId(project.id)) {
      issues.push({
        path: `${prefix}.id`,
        message: getConfigProjectIdValidationMessage(),
      });
    } else if (projectIds.has(project.id)) {
      issues.push({
        path: `${prefix}.id`,
        message: `duplicate project id: ${project.id}`,
      });
    } else {
      projectIds.add(project.id);
    }

    if (!isNonEmptyString(project.name)) {
      issues.push({
        path: `${prefix}.name`,
        message: "must be a non-empty string",
      });
    }

    if (!isNonEmptyString(project.repoPath)) {
      issues.push({
        path: `${prefix}.repoPath`,
        message: "must be a non-empty path",
      });
    }
  }

  if (issues.length === 0) {
    await Promise.all([
      ensureWritablePath(
        config.storage.dbPath,
        "file-parent",
        issues,
        "storage.dbPath",
      ),
      ensureWritablePath(
        config.daemon.logDir,
        "directory",
        issues,
        "daemon.logDir",
      ),
      ensureWritablePath(
        config.daemon.workingDirectory,
        "directory",
        issues,
        "daemon.workingDirectory",
      ),
      ensureWritablePath(
        options.defaultWorktreeRoot ?? getDefaultWorktreeRoot(),
        "directory",
        issues,
        "defaults.worktreeRoot",
      ),
    ]);
  }

  if (issues.length > 0) {
    throw new ConfigValidationError(issues);
  }
}
