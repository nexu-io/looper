import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  ConfigValidationError,
  createDefaultLooperConfig,
  loadLooperConfig,
  validateLooperConfig,
} from "./index";

async function createFixture(): Promise<{
  rootDir: string;
  configPath: string;
  writableDir: string;
  dbPath: string;
  logDir: string;
}> {
  const rootDir = await mkdtemp(join(tmpdir(), "looper-config-"));
  const writableDir = join(rootDir, "workspace");
  const dbDir = join(rootDir, "db");
  const logDir = join(rootDir, "logs");
  const configPath = join(rootDir, "config.json");
  const dbPath = join(dbDir, "looper.sqlite");

  await Promise.all([
    mkdir(writableDir, { recursive: true }),
    mkdir(dbDir, { recursive: true }),
    mkdir(logDir, { recursive: true }),
  ]);

  return { rootDir, configPath, writableDir, dbPath, logDir };
}

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

describe("loadLooperConfig", () => {
  test("defaults scheduler maxConcurrentRuns to 3", () => {
    const config = createDefaultLooperConfig();
    expect(config.scheduler.maxConcurrentRuns).toBe(3);
  });

  test("merges config file, env, and CLI overrides in priority order", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    await writeFile(
      fixture.configPath,
      JSON.stringify({
        server: { host: "0.0.0.0", port: 5555 },
        daemon: {
          logDir: fixture.logDir,
          workingDirectory: fixture.writableDir,
        },
        storage: { dbPath: fixture.dbPath },
        notifications: {
          osascript: { enabled: false, throttleWindowSeconds: 60 },
        },
        tools: {
          bunPath: "/file/bun",
          gitPath: "/file/git",
          ghPath: "/file/gh",
        },
      }),
    );

    const loaded = await loadLooperConfig({
      argv: [
        "--config",
        fixture.configPath,
        "--port",
        "7000",
        "--git-path",
        "/cli/git",
        "--allow-auto-commit",
        "false",
        "--allow-auto-push",
        "false",
      ],
      cwd: fixture.rootDir,
      env: {
        LOOPER_HOST: "127.0.0.2",
        LOOPER_DB_PATH: join(fixture.rootDir, "env.sqlite"),
        LOOPER_LOG_DIR: join(fixture.rootDir, "env-logs"),
        LOOPER_OSASCRIPT_ENABLED: "false",
        LOOPER_IN_APP_NOTIFICATIONS: "false",
        LOOPER_BUN_PATH: "/env/bun",
        LOOPER_GH_PATH: "/env/gh",
        LOOPER_ALLOW_AUTO_COMMIT: "true",
        LOOPER_ALLOW_AUTO_APPROVE: "true",
      },
    });

    expect(loaded.config.server.host).toBe("127.0.0.2");
    expect(loaded.config.server.port).toBe(7000);
    expect(loaded.config.storage.dbPath).toBe(
      join(fixture.rootDir, "env.sqlite"),
    );
    expect(loaded.config.daemon.logDir).toBe(join(fixture.rootDir, "env-logs"));
    expect(loaded.config.tools.bunPath).toBe("/env/bun");
    expect(loaded.config.tools.gitPath).toBe("/cli/git");
    expect(loaded.config.tools.ghPath).toBe("/env/gh");
    expect(loaded.config.agent.vendor).toBeUndefined();
    expect(loaded.config.defaults.allowAutoCommit).toBe(false);
    expect(loaded.config.defaults.allowAutoPush).toBe(false);
    expect(loaded.config.defaults.allowAutoApprove).toBe(true);
    expect(loaded.config.defaults.openPrStrategy).toBe("manual");
    expect(loaded.config.notifications.inApp).toBe(false);
    expect(loaded.config.notifications.osascript.enabled).toBe(false);
    expect(loaded.metadata.configFilePresent).toBe(true);
  });

  test("throws validation errors for unsupported config", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    await writeFile(
      fixture.configPath,
      JSON.stringify({
        server: { port: 0 },
        daemon: {
          logDir: fixture.logDir,
          workingDirectory: fixture.writableDir,
        },
        storage: { dbPath: fixture.dbPath },
        scheduler: { pollIntervalSeconds: 2 },
        notifications: {
          osascript: { enabled: true, throttleWindowSeconds: 60 },
        },
        defaults: {
          allowAutoCommit: "yes",
        },
      }),
    );

    expect(
      loadLooperConfig({
        argv: ["--config", fixture.configPath],
        cwd: fixture.rootDir,
        env: {
          LOOPER_BUN_PATH: "/env/bun",
          LOOPER_GIT_PATH: "/env/git",
          LOOPER_GH_PATH: "/env/gh",
        },
      }),
    ).rejects.toBeInstanceOf(ConfigValidationError);
  });

  test("rejects unknown CLI flags instead of prefix matching them", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    await writeFile(
      fixture.configPath,
      JSON.stringify({
        daemon: {
          logDir: fixture.logDir,
          workingDirectory: fixture.writableDir,
        },
        storage: { dbPath: fixture.dbPath },
        notifications: {
          osascript: { enabled: false, throttleWindowSeconds: 60 },
        },
        tools: {
          bunPath: "/file/bun",
          gitPath: "/file/git",
          ghPath: "/file/gh",
        },
      }),
    );

    expect(
      loadLooperConfig({
        argv: ["--config", fixture.configPath, "--hostfoo", "127.0.0.99"],
        cwd: fixture.rootDir,
        env: {},
      }),
    ).rejects.toThrow("Unknown looperd argument: --hostfoo");
  });

  test("rejects project ids that would escape the default worktree root", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    await writeFile(
      fixture.configPath,
      JSON.stringify({
        daemon: {
          logDir: fixture.logDir,
          workingDirectory: fixture.writableDir,
        },
        storage: { dbPath: fixture.dbPath },
        notifications: {
          osascript: { enabled: false, throttleWindowSeconds: 60 },
        },
        tools: {
          bunPath: "/file/bun",
          gitPath: "/file/git",
          ghPath: "/file/gh",
        },
        projects: [
          {
            id: "../../tmp",
            name: "bad-project",
            repoPath: fixture.writableDir,
          },
        ],
      }),
    );

    await expect(
      loadLooperConfig({
        argv: ["--config", fixture.configPath],
        cwd: fixture.rootDir,
        env: {},
      }),
    ).rejects.toMatchObject({
      issues: [
        {
          path: "projects[0].id",
          message:
            "must not contain path separators, dot segments, or be an absolute path",
        },
      ],
    });
  });

  test("allows legacy project ids in config for upgrade compatibility", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    await writeFile(
      fixture.configPath,
      JSON.stringify({
        daemon: {
          logDir: fixture.logDir,
          workingDirectory: fixture.writableDir,
        },
        storage: { dbPath: fixture.dbPath },
        notifications: {
          osascript: { enabled: false, throttleWindowSeconds: 60 },
        },
        tools: {
          bunPath: "/file/bun",
          gitPath: "/file/git",
          ghPath: "/file/gh",
        },
        projects: [
          {
            id: "legacy-id-Li4vdG1w",
            name: "legacy-project",
            repoPath: fixture.writableDir,
          },
        ],
      }),
    );

    const loaded = await loadLooperConfig({
      argv: ["--config", fixture.configPath],
      cwd: fixture.rootDir,
      env: {},
    });

    expect(loaded.config.projects[0]?.id).toBe("legacy-id-Li4vdG1w");
  });

  test("rejects configs when the default worktree root cannot be created", async () => {
    const fixture = await createFixture();
    cleanupPaths.push(fixture.rootDir);

    const blockingRoot = join(fixture.rootDir, "blocked-root");
    await writeFile(blockingRoot, "not-a-directory");

    const config = createDefaultLooperConfig(fixture.rootDir);
    config.daemon.logDir = fixture.logDir;
    config.daemon.workingDirectory = fixture.writableDir;
    config.storage.dbPath = fixture.dbPath;
    config.notifications.osascript.enabled = false;
    config.tools = {
      bunPath: "/file/bun",
      gitPath: "/file/git",
      ghPath: "/file/gh",
    };

    await expect(
      validateLooperConfig(config, {
        defaultWorktreeRoot: join(blockingRoot, "worktrees"),
      }),
    ).rejects.toMatchObject({
      issues: [
        {
          path: "defaults.worktreeRoot",
          message: `${blockingRoot} is not a directory`,
        },
      ],
    });
  });
});
