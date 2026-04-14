import { homedir } from "node:os";
import { join } from "node:path";

import { assertValidProjectId } from "./project-id";
import type { LooperConfig } from "./types";

const LOOPER_HOME = join(homedir(), ".looper");
const DEFAULT_WORKTREE_ROOT = join(LOOPER_HOME, "worktrees");

export function createDefaultLooperConfig(cwd = process.cwd()): LooperConfig {
  return {
    server: {
      host: "127.0.0.1",
      port: 4310,
      authMode: "none",
    },
    storage: {
      mode: "sqlite",
      dbPath: join(LOOPER_HOME, "looper.sqlite"),
      backupDir: join(LOOPER_HOME, "backups"),
    },
    scheduler: {
      pollIntervalSeconds: 30,
      maxConcurrentRuns: 1,
      retryMaxAttempts: 5,
      retryBaseDelayMs: 5_000,
    },
    agent: {
      env: {},
      params: {},
    },
    logging: {
      level: "info",
      maxSizeMB: 10,
      maxFiles: 5,
    },
    notifications: {
      inApp: true,
      osascript: {
        enabled: true,
        soundForLevels: ["action_required", "failure"],
        throttleWindowSeconds: 60,
      },
    },
    tools: {},
    daemon: {
      mode: "foreground",
      logDir: join(LOOPER_HOME, "logs"),
      workingDirectory: cwd,
      environment: {},
    },
    package: {
      distribution: "npm",
      autoMigrateOnStartup: true,
      requireBackupBeforeMigrate: false,
    },
    defaults: {
      baseBranch: "main",
      allowAutoCommit: true,
      allowAutoPush: true,
      allowAutoApprove: false,
      allowAutoMerge: false,
      allowRiskyFixes: false,
      openPrStrategy: "manual",
    },
    projects: [],
  };
}

export function getDefaultWorktreeRoot(): string {
  return DEFAULT_WORKTREE_ROOT;
}

export function getDefaultProjectWorktreeRoot(projectId: string): string {
  assertValidProjectId(projectId);
  return join(DEFAULT_WORKTREE_ROOT, projectId);
}

export function getDefaultConfigPath(): string {
  return join(LOOPER_HOME, "config.json");
}
