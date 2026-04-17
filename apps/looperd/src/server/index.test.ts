import { describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "../bootstrap/logger";
import {
  InvalidProjectIdError,
  createDefaultLooperConfig,
} from "../config/index";
import { LOOPERD_BUILD_METADATA, LOOPERD_VERSION } from "../metadata";
import { ProjectIdCollisionError } from "../projects/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import { createLooperdApi } from "./index";

const DAEMON_HTTP_CONTRACT_ARTIFACT_PATH = new URL(
  "../../../../specs/2026-04-17-go-port-plan/artifacts/daemon-http.compat.json",
  import.meta.url,
);

const DAEMON_HTTP_RESPONSE_ARTIFACT_PATH = new URL(
  "../../../../specs/2026-04-17-go-port-plan/artifacts/daemon-http.responses.compat.json",
  import.meta.url,
);

const DAEMON_HTTP_REQUEST_ARTIFACT_PATH = new URL(
  "../../../../specs/2026-04-17-go-port-plan/artifacts/daemon-http.requests.compat.json",
  import.meta.url,
);

const DAEMON_HTTP_ERROR_ARTIFACT_PATH = new URL(
  "../../../../specs/2026-04-17-go-port-plan/artifacts/daemon-http.errors.compat.json",
  import.meta.url,
);

const SEEDED_NOW = "2026-04-11T12:00:00.000Z";
const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ISO_TIMESTAMP_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;

interface DaemonHttpContractArtifact {
  sharedBehavior: {
    responseHeaders: Record<string, string>;
    requestIdHeader: { name: string };
    auth: {
      localTokenMode: {
        missingConfiguredToken: { status: number };
        missingOrWrongBearerToken: { status: number };
        matchingBearerToken: { status: number };
      };
    };
    genericStatuses: {
      routeNotFound: number;
      methodNotAllowed: number;
    };
  };
  routes: Array<{
    id: string;
    method: string;
    path: string;
    fixture: "default" | "projects-enabled" | "runtime-control";
    request?: {
      headers?: Record<string, string>;
      body?: Record<string, unknown>;
    };
    expectedStatus: number;
  }>;
}

interface DaemonHttpResponseArtifact {
  routes: Array<{
    id: string;
    status: number;
    body: unknown;
  }>;
}

interface DaemonHttpRequestArtifact {
  routes: Array<{
    id: string;
    method: string;
    path: string;
    request: {
      headers?: Record<string, string>;
      body?: Record<string, unknown>;
    };
  }>;
}

interface DaemonHttpErrorArtifact {
  sharedBehavior: {
    responseHeaders: Record<string, string>;
    requestIdHeader: { name: string };
  };
  cases: Array<{
    id: string;
    fixture:
      | "default"
      | "projects-id-conflict"
      | "projects-internal-error"
      | "projects-invalid-id"
      | "auth-misconfigured"
      | "auth-required"
      | "runtime-control-disabled"
      | "inactive-run"
      | "ambiguous-project-repo"
      | "pull-request-mismatch";
    method: string;
    path: string;
    request?: {
      headers?: Record<string, string>;
      body?: Record<string, unknown>;
    };
    expectedStatus: number;
    body: unknown;
  }>;
}

async function loadDaemonHttpContractArtifact() {
  return (await Bun.file(
    DAEMON_HTTP_CONTRACT_ARTIFACT_PATH,
  ).json()) as DaemonHttpContractArtifact;
}

async function loadDaemonHttpResponseArtifact() {
  return (await Bun.file(
    DAEMON_HTTP_RESPONSE_ARTIFACT_PATH,
  ).json()) as DaemonHttpResponseArtifact;
}

async function loadDaemonHttpRequestArtifact() {
  return (await Bun.file(
    DAEMON_HTTP_REQUEST_ARTIFACT_PATH,
  ).json()) as DaemonHttpRequestArtifact;
}

async function loadDaemonHttpErrorArtifact() {
  return (await Bun.file(
    DAEMON_HTTP_ERROR_ARTIFACT_PATH,
  ).json()) as DaemonHttpErrorArtifact;
}

function normalizeResponseFixture(
  value: unknown,
  options: { rootDir: string },
  path = "",
): unknown {
  if (typeof value === "string") {
    const normalized = value
      .replaceAll(options.rootDir, "<tmp-root>")
      .replaceAll(homedir(), "<home>");

    if (path.endsWith(".currentTarget")) {
      return "<current-target>";
    }

    if (path.endsWith(".artifactName") && normalized.length > 0) {
      return "<artifact-name>";
    }

    if (UUID_PATTERN.test(normalized)) {
      return "<uuid>";
    }

    if (ISO_TIMESTAMP_PATTERN.test(normalized) && normalized !== SEEDED_NOW) {
      return "<generated-timestamp>";
    }

    return normalized;
  }

  if (Array.isArray(value)) {
    return value.map((item, index) =>
      normalizeResponseFixture(item, options, `${path}[${index}]`),
    );
  }

  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, item]) => [
        key,
        normalizeResponseFixture(item, options, path ? `${path}.${key}` : key),
      ]),
    );
  }

  return value;
}

async function createFixture(options?: {
  runtimeControl?: {
    stopLoop(input: { loopId: string; reason: string }): Promise<unknown>;
    triggerSchedulerTick(): void;
  };
}) {
  const rootDir = await mkdtemp(join(tmpdir(), "looperd-api-"));
  const config = createDefaultLooperConfig(rootDir);
  config.storage.dbPath = `${rootDir}/state/looper.sqlite`;
  config.storage.backupDir = `${rootDir}/backups`;
  config.daemon.logDir = `${rootDir}/logs`;
  config.daemon.workingDirectory = rootDir;
  config.server.authMode = "none";
  config.agent.vendor = "opencode";
  config.tools.bunPath = "/usr/bin/bun";
  config.tools.gitPath = "/usr/bin/git";
  config.tools.ghPath = "/usr/bin/gh";
  config.tools.osascriptPath = "/usr/bin/osascript";

  const logger = await createLogger(config.logging, config.daemon.logDir);
  const store = new SqliteStore({
    dbPath: config.storage.dbPath,
    backupDir: config.storage.backupDir,
  });
  store.initialize({ autoMigrate: true });

  const now = "2026-04-11T12:00:00.000Z";
  store.projects.upsert({
    id: "project_1",
    name: "Looper",
    repoPath: "/tmp/looper",
    baseBranch: "main",
    archived: false,
    metadataJson: JSON.stringify({ repo: "acme/looper" }),
    createdAt: now,
    updatedAt: now,
  });
  store.loops.upsert({
    id: "loop_1",
    seq: 1,
    projectId: "project_1",
    type: "reviewer",
    targetType: "pull_request",
    targetId: "pr:acme/looper:42",
    repo: "acme/looper",
    prNumber: 42,
    status: "running",
    configJson: null,
    metadataJson: null,
    lastRunAt: now,
    nextRunAt: now,
    createdAt: now,
    updatedAt: now,
  });
  store.runs.upsert({
    id: "run_1",
    loopId: "loop_1",
    status: "running",
    currentStep: "review",
    lastCompletedStep: "snapshot",
    checkpointJson: null,
    summary: null,
    errorMessage: null,
    startedAt: now,
    lastHeartbeatAt: now,
    endedAt: null,
    createdAt: now,
    updatedAt: now,
  });
  store.queue.upsert({
    id: "queue_1",
    projectId: "project_1",
    loopId: "loop_1",
    type: "worker",
    targetType: "project",
    targetId: "project_1",
    repo: "acme/looper",
    prNumber: 42,
    dedupeKey: "worker:loop_1",
    priority: 3,
    status: "queued",
    availableAt: now,
    attempts: 0,
    maxAttempts: 3,
    claimedBy: null,
    claimedAt: null,
    startedAt: null,
    finishedAt: null,
    lockKey: "worker:loop_1",
    payloadJson: JSON.stringify({
      title: "Wire runtime",
      repo: "acme/looper",
      baseBranch: "main",
      prompt: "Wire runtime",
    }),
    lastError: null,
    lastErrorKind: null,
    createdAt: now,
    updatedAt: now,
  });
  store.pullRequestSnapshots.upsert({
    id: "snapshot_1",
    projectId: "project_1",
    repo: "acme/looper",
    prNumber: 42,
    headSha: "abc123",
    baseSha: "base123",
    title: "Runtime foundation",
    body: "Adds recovery and API",
    author: "octocat",
    diffRef: null,
    checksSummary: "green",
    unresolvedThreadCount: 1,
    reviewState: "changes_requested",
    payloadJson: JSON.stringify({ title: "Runtime foundation" }),
    capturedAt: now,
    createdAt: now,
  });
  store.events.append({
    id: "event_1",
    eventType: "loop.created",
    projectId: "project_1",
    loopId: "loop_1",
    runId: null,
    entityType: "loop",
    entityId: "loop_1",
    correlationId: null,
    causationId: null,
    actorType: "system",
    actorId: "looperd",
    actorDisplayName: "looperd",
    payloadJson: JSON.stringify({ status: "running" }),
    createdAt: now,
  });

  const api = createLooperdApi({
    config,
    logger,
    store,
    getStartedAt: () => new Date(now),
    getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    runtimeControl: options?.runtimeControl as
      | {
          stopLoop(input: {
            loopId: string;
            reason: string;
          }): Promise<{
            stopped: boolean;
            loopId: string;
            runId?: string;
            executionId?: string;
            vendor?: string;
            pid?: number | null;
          }>;
          triggerSchedulerTick(): void;
        }
      | undefined,
  });

  return { api, store, rootDir, config, logger };
}

describe("createLooperdApi", () => {
  test("returns status and config envelopes", async () => {
    const { api, store, rootDir } = await createFixture();

    const statusResponse = await api.handle(
      new Request("http://localhost/api/v1/status"),
    );
    const statusBody = (await statusResponse.json()) as {
      ok: boolean;
      data: {
        service: {
          version: string;
          build: {
            versionSource: string;
            gitCommitSha: string | null;
            buildTimestamp: string | null;
          };
          binary: { installDir: string; supportedTargets: string[] };
        };
        storage: { schemaVersion: string };
        scheduler: { queuedItems: number; totalRuns: number };
        loops: { planner: { running: number } };
        safety: {
          allowAutoCommit: boolean;
          allowAutoPush: boolean;
          allowAutoApprove: boolean;
          allowRiskyFixes: boolean;
          openPrStrategy: string;
        };
        notifications: { inAppEnabled: boolean };
      };
    };

    expect(statusResponse.status).toBe(200);
    expect(statusBody.ok).toBe(true);
    expect(statusBody.data.service.version).toBe(LOOPERD_VERSION);
    expect(statusBody.data.service.build).toEqual(LOOPERD_BUILD_METADATA);
    expect(statusBody.data.service.binary.installDir).toBe(
      join(homedir(), ".looper", "bin"),
    );
    expect(statusBody.data.service.binary.supportedTargets).toEqual([
      "darwin-arm64",
      "darwin-x64",
    ]);
    expect(statusBody.data.storage.schemaVersion).toBe(
      "0007_agent_execution_run_index",
    );
    expect(statusBody.data.scheduler.queuedItems).toBe(1);
    expect(statusBody.data.scheduler.totalRuns).toBe(1);
    expect(statusBody.data.loops.planner.running).toBe(0);
    expect(statusBody.data.safety.allowAutoCommit).toBe(true);
    expect(statusBody.data.safety.allowAutoPush).toBe(true);
    expect(statusBody.data.safety.allowAutoApprove).toBe(false);
    expect(statusBody.data.safety.allowRiskyFixes).toBe(false);
    expect(statusBody.data.safety.openPrStrategy).toBe("manual");
    expect(statusBody.data.notifications.inAppEnabled).toBe(true);

    const configResponse = await api.handle(
      new Request("http://localhost/api/v1/config"),
    );
    const configBody = (await configResponse.json()) as {
      data: { server: { localTokenConfigured: boolean } };
    };
    expect(configBody.data.server.localTokenConfigured).toBe(false);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("returns events and pull request detail routes", async () => {
    const { api, store, rootDir } = await createFixture();

    store.loops.upsert({
      id: "loop_2",
      seq: 2,
      projectId: "project_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "paused",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: null,
      createdAt: "2026-04-11T12:01:00.000Z",
      updatedAt: "2026-04-11T12:01:00.000Z",
    });

    const eventsResponse = await api.handle(
      new Request("http://localhost/api/v1/events/loop/loop_1"),
    );
    const eventsBody = (await eventsResponse.json()) as {
      data: { items: Array<{ eventType: string }> };
    };
    expect(eventsBody.data.items).toHaveLength(1);
    expect(eventsBody.data.items[0]?.eventType).toBe("loop.created");

    const prResponse = await api.handle(
      new Request("http://localhost/api/v1/pull-requests/acme%2Flooper/42"),
    );
    const prBody = (await prResponse.json()) as {
      data: { repo: string; prNumber: number };
    };
    expect(prBody.data.repo).toBe("acme/looper");
    expect(prBody.data.prNumber).toBe(42);
    expect((prBody.data as { reviewer?: string }).reviewer).toBe("running");
    expect((prBody.data as { fixer?: string }).fixer).toBe("paused");

    const prListResponse = await api.handle(
      new Request("http://localhost/api/v1/pull-requests"),
    );
    const prListBody = (await prListResponse.json()) as {
      data: {
        items: Array<{
          repo: string;
          prNumber: number;
          reviewState: string | null;
          checksSummary: string | null;
          reviewer: string | null;
          fixer: string | null;
        }>;
      };
    };
    const listItem = prListBody.data.items.find(
      (item) => item.repo === "acme/looper" && item.prNumber === 42,
    );
    expect(listItem?.reviewState).toBe("changes_requested");
    expect(listItem?.checksSummary).toBe("green");
    expect(listItem?.reviewer).toBe("running");
    expect(listItem?.fixer).toBe("paused");

    const prStatusResponse = await api.handle(
      new Request(
        "http://localhost/api/v1/pull-requests/acme%2Flooper/42/status",
      ),
    );
    const prStatusBody = (await prStatusResponse.json()) as {
      data: {
        loopStatus: { latestRunStatus: string };
        reviewer: string | null;
        fixer: string | null;
      };
    };
    expect(prStatusBody.data.loopStatus.latestRunStatus).toBe("running");
    expect(prStatusBody.data.reviewer).toBe("running");
    expect(prStatusBody.data.fixer).toBe("paused");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("lists PR identities from loops when snapshot is missing", async () => {
    const { api, store, rootDir } = await createFixture();

    store.loops.upsert({
      id: "loop_no_snapshot",
      seq: 3,
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:77",
      repo: "acme/looper",
      prNumber: 77,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: null,
      createdAt: "2026-04-11T12:02:00.000Z",
      updatedAt: "2026-04-11T12:02:00.000Z",
    });

    const prListResponse = await api.handle(
      new Request("http://localhost/api/v1/pull-requests"),
    );
    const prListBody = (await prListResponse.json()) as {
      data: {
        items: Array<{
          repo: string;
          prNumber: number;
          reviewState: string | null;
          checksSummary: string | null;
          reviewer: string | null;
          fixer: string | null;
        }>;
      };
    };

    const missingSnapshotItem = prListBody.data.items.find(
      (item) => item.repo === "acme/looper" && item.prNumber === 77,
    );
    expect(missingSnapshotItem).toBeDefined();
    expect(missingSnapshotItem?.reviewState).toBeNull();
    expect(missingSnapshotItem?.checksSummary).toBeNull();
    expect(missingSnapshotItem?.reviewer).toBe("queued");
    expect(missingSnapshotItem?.fixer).toBeNull();

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("returns loop and run read routes", async () => {
    const { api, store, rootDir } = await createFixture();

    const loopsResponse = await api.handle(
      new Request("http://localhost/api/v1/loops"),
    );
    const loopsBody = (await loopsResponse.json()) as {
      data: { items: Array<{ id: string }> };
    };
    expect(loopsResponse.status).toBe(200);
    expect(loopsBody.data.items[0]?.id).toBe("loop_1");

    const loopResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/loop_1"),
    );
    const loopBody = (await loopResponse.json()) as {
      data: { id: string; status: string };
    };
    expect(loopBody.data.id).toBe("loop_1");
    expect(loopBody.data.status).toBe("running");

    const runsResponse = await api.handle(
      new Request("http://localhost/api/v1/runs"),
    );
    const runsBody = (await runsResponse.json()) as {
      data: { items: Array<{ id: string }> };
    };
    expect(runsResponse.status).toBe(200);
    expect(runsBody.data.items[0]?.id).toBe("run_1");

    const filteredRunsResponse = await api.handle(
      new Request("http://localhost/api/v1/runs?loopId=loop_1"),
    );
    const filteredRunsBody = (await filteredRunsResponse.json()) as {
      data: { items: Array<{ loopId: string }> };
    };
    expect(filteredRunsBody.data.items).toHaveLength(1);
    expect(filteredRunsBody.data.items[0]?.loopId).toBe("loop_1");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("supports loop and worker mutation routes", async () => {
    let schedulerTriggerCalls = 0;
    const { api, store, rootDir } = await createFixture({
      runtimeControl: {
        stopLoop: async () => ({ stopped: false }),
        triggerSchedulerTick: () => {
          schedulerTriggerCalls += 1;
        },
      },
    });

    const pauseLoopResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/loop_1/pause", {
        method: "POST",
      }),
    );
    const pauseLoopBody = (await pauseLoopResponse.json()) as {
      data: { status: string };
    };
    expect(pauseLoopResponse.status).toBe(200);
    expect(pauseLoopBody.data.status).toBe("paused");

    const startLoopResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/loop_1/start", {
        method: "POST",
      }),
    );
    const startLoopBody = (await startLoopResponse.json()) as {
      data: { status: string };
    };
    expect(startLoopBody.data.status).toBe("running");

    const createWorkerResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          title: "Implement CLI route",
          prompt: "Create API endpoint",
          specPath: "specs/cli.md",
          repo: "acme/looper",
          baseBranch: "main",
        }),
      }),
    );
    const createWorkerBody = (await createWorkerResponse.json()) as {
      data: {
        id: string;
        status: string;
        title: string;
        specPath: string;
      };
    };
    expect(createWorkerResponse.status).toBe(200);
    expect(createWorkerBody.data.status).toBe("queued");
    expect(createWorkerBody.data.title).toBe("Implement CLI route");
    expect(createWorkerBody.data.specPath).toBe("specs/cli.md");
    expect(
      store.queue.findActiveByDedupe(`worker:${createWorkerBody.data.id}`),
    ).toMatchObject({
      loopId: createWorkerBody.data.id,
      type: "worker",
      status: "queued",
    });
    expect(schedulerTriggerCalls).toBe(1);

    const createSecondProjectWorkerResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          title: "Add CLI list command",
          prompt: "Add a list subcommand",
          repo: "acme/looper",
          baseBranch: "main",
        }),
      }),
    );
    const createSecondProjectWorkerBody =
      (await createSecondProjectWorkerResponse.json()) as {
        data: {
          id: string;
          status: string;
          title: string;
        };
      };
    expect(createSecondProjectWorkerResponse.status).toBe(200);
    expect(createSecondProjectWorkerBody.data.status).toBe("queued");
    expect(createSecondProjectWorkerBody.data.title).toBe(
      "Add CLI list command",
    );
    expect(createSecondProjectWorkerBody.data.id).not.toBe(
      createWorkerBody.data.id,
    );
    expect(
      store.queue.findActiveByDedupe(
        `worker:${createSecondProjectWorkerBody.data.id}`,
      ),
    ).toMatchObject({
      loopId: createSecondProjectWorkerBody.data.id,
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      status: "queued",
    });

    const createWorkerFromPrResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          repo: "acme/looper",
          prNumber: 42,
        }),
      }),
    );
    const createWorkerFromPrBody =
      (await createWorkerFromPrResponse.json()) as {
        data: {
          id: string;
          status: string;
          repo: string;
          prNumber: number;
        };
      };
    expect(createWorkerFromPrResponse.status).toBe(200);
    expect(createWorkerFromPrBody.data.status).toBe("queued");
    expect(createWorkerFromPrBody.data.repo).toBe("acme/looper");
    expect(createWorkerFromPrBody.data.prNumber).toBe(42);
    expect(
      store.queue.findActiveByDedupe("worker:project_1:acme/looper:42"),
    ).toMatchObject({
      loopId: createWorkerFromPrBody.data.id,
      type: "worker",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      prNumber: 42,
      lockKey: "pr:acme/looper:42",
      status: "queued",
    });

    const duplicatePrWorkerResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          repo: "acme/looper",
          prNumber: 42,
        }),
      }),
    );
    const duplicatePrWorkerBody = (await duplicatePrWorkerResponse.json()) as {
      error: { code: string; message: string };
    };
    expect(duplicatePrWorkerResponse.status).toBe(409);
    expect(duplicatePrWorkerBody.error.code).toBe("LOOP_CONFLICT");

    store.projects.upsert({
      id: "project_2",
      name: "Looper mirror",
      repoPath: "/tmp/looper-mirror",
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "acme/looper" }),
      createdAt: "2026-04-11T12:04:00.000Z",
      updatedAt: "2026-04-11T12:04:00.000Z",
    });

    const createWorkerFromSamePrSecondProjectResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_2",
          repo: "acme/looper",
          prNumber: 42,
        }),
      }),
    );
    const createWorkerFromSamePrSecondProjectBody =
      (await createWorkerFromSamePrSecondProjectResponse.json()) as {
        data: {
          id: string;
          status: string;
          repo: string;
          prNumber: number;
        };
      };
    expect(createWorkerFromSamePrSecondProjectResponse.status).toBe(200);
    expect(
      store.queue.findActiveByDedupe("worker:project_2:acme/looper:42"),
    ).toMatchObject({
      loopId: createWorkerFromSamePrSecondProjectBody.data.id,
      projectId: "project_2",
      type: "worker",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      prNumber: 42,
      lockKey: "pr:acme/looper:42",
      status: "queued",
    });

    store.pullRequestSnapshots.upsert({
      id: "snapshot_2",
      projectId: "project_2",
      repo: "acme/looper",
      prNumber: 42,
      headSha: "def456",
      baseSha: "base123",
      title: "Runtime foundation",
      body: "Adds recovery and API",
      author: "octocat",
      diffRef: null,
      checksSummary: "green",
      unresolvedThreadCount: 1,
      reviewState: "changes_requested",
      payloadJson: JSON.stringify({ title: "Runtime foundation" }),
      capturedAt: "2026-04-11T12:05:00.000Z",
      createdAt: "2026-04-11T12:05:00.000Z",
    });

    const ambiguousPrWorkerResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repo: "acme/looper",
          prNumber: 42,
        }),
      }),
    );
    const ambiguousPrWorkerBody = (await ambiguousPrWorkerResponse.json()) as {
      error: { code: string; message: string };
    };
    expect(ambiguousPrWorkerResponse.status).toBe(409);
    expect(ambiguousPrWorkerBody.error.code).toBe("PROJECT_AMBIGUOUS");

    store.loops.upsert({
      id: "loop_planner_issue_125",
      seq: 99,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:125",
      repo: "acme/looper",
      prNumber: 77,
      status: "running",
      configJson: null,
      metadataJson: JSON.stringify({
        prNumber: 77,
        specPath: "specs/issue-125.md",
      }),
      lastRunAt: "2026-04-11T12:03:00.000Z",
      nextRunAt: "2026-04-11T12:03:00.000Z",
      createdAt: "2026-04-11T12:03:00.000Z",
      updatedAt: "2026-04-11T12:03:00.000Z",
    });

    const createWorkerFromIssueResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          issueNumber: 125,
        }),
      }),
    );
    const createWorkerFromIssueBody =
      (await createWorkerFromIssueResponse.json()) as {
        data: {
          id: string;
          status: string;
          issueNumber: number;
          prNumber: number;
          specPath: string;
        };
      };
    expect(createWorkerFromIssueResponse.status).toBe(200);
    expect(createWorkerFromIssueBody.data.status).toBe("queued");
    expect(createWorkerFromIssueBody.data.issueNumber).toBe(125);
    expect(createWorkerFromIssueBody.data.prNumber).toBe(77);
    expect(createWorkerFromIssueBody.data.specPath).toBe("specs/issue-125.md");
    expect(
      store.queue.findActiveByDedupe("worker:project_1:acme/looper:77"),
    ).toMatchObject({
      loopId: createWorkerFromIssueBody.data.id,
      type: "worker",
      targetType: "pull_request",
      targetId: "pr:acme/looper:77",
      prNumber: 77,
      lockKey: "pr:acme/looper:77",
      status: "queued",
    });

    const validationResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ projectId: "project_1", title: "" }),
      }),
    );
    expect(validationResponse.status).toBe(400);

    const missingPrResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          repo: "acme/looper",
          prNumber: 999,
        }),
      }),
    );
    expect(missingPrResponse.status).toBe(404);

    const missingIssueResponse = await api.handle(
      new Request("http://localhost/api/v1/workers", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_2",
          repo: "acme/looper",
          issueNumber: 999,
        }),
      }),
    );
    const missingIssueBody = (await missingIssueResponse.json()) as {
      data: {
        id: string;
        status: string;
        issueNumber: number;
        prNumber?: number;
        specPath?: string;
      };
    };
    expect(missingIssueResponse.status).toBe(200);
    expect(missingIssueBody.data.status).toBe("queued");
    expect(missingIssueBody.data.issueNumber).toBe(999);
    expect(missingIssueBody.data.prNumber).toBeNull();
    expect(missingIssueBody.data.specPath).toBeNull();
    expect(
      store.queue.findActiveByDedupe(`worker:${missingIssueBody.data.id}`),
    ).toMatchObject({
      loopId: missingIssueBody.data.id,
      type: "worker",
      targetType: "project",
      targetId: "project_2",
      status: "queued",
    });

    const createLoopResponse = await api.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "fixer",
          targetType: "pull_request",
          repo: "acme/looper",
          prNumber: 43,
        }),
      }),
    );
    const createLoopBody = (await createLoopResponse.json()) as {
      data: { type: string; repo: string; prNumber: number; status: string };
    };
    expect(createLoopResponse.status).toBe(200);
    expect(createLoopBody.data.type).toBe("fixer");
    expect(createLoopBody.data.repo).toBe("acme/looper");
    expect(createLoopBody.data.prNumber).toBe(43);
    expect(createLoopBody.data.status).toBe("running");

    const createReviewerResponse = await api.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "reviewer",
          targetType: "pull_request",
          repo: "acme/looper",
          prNumber: 43,
          metadata: { followUpdates: true, manual: true },
        }),
      }),
    );
    const createReviewerBody = (await createReviewerResponse.json()) as {
      data: { id: string; metadataJson: string | null; status: string };
    };
    expect(createReviewerResponse.status).toBe(200);
    expect(createReviewerBody.data.status).toBe("running");
    expect(createReviewerBody.data.metadataJson).toContain(
      '"followUpdates":true',
    );
    expect(
      store.queue.findActiveByDedupe("reviewer:acme/looper:43"),
    ).toMatchObject({
      loopId: createReviewerBody.data.id,
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:43",
      prNumber: 43,
      status: "queued",
    });

    const createPausedReviewerResponse = await api.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "reviewer",
          targetType: "pull_request",
          repo: "acme/looper",
          prNumber: 44,
          status: "paused",
          metadata: { followUpdates: true, manual: true },
        }),
      }),
    );
    const createPausedReviewerBody =
      (await createPausedReviewerResponse.json()) as {
        data: { id: string; status: string };
      };
    expect(createPausedReviewerResponse.status).toBe(200);
    expect(createPausedReviewerBody.data.status).toBe("paused");
    expect(
      store.queue.findActiveByDedupe("reviewer:acme/looper:44"),
    ).toBeNull();

    const createPlannerResponse = await api.handle(
      new Request("http://localhost/api/v1/planners", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          issueNumber: 123,
        }),
      }),
    );
    const createPlannerBody = (await createPlannerResponse.json()) as {
      data: { type: string; issueNumber: number; status: string };
    };
    expect(createPlannerResponse.status).toBe(200);
    expect(createPlannerBody.data.type).toBe("planner");
    expect(createPlannerBody.data.issueNumber).toBe(123);
    expect(createPlannerBody.data.status).toBe("running");

    const createGenericPlannerResponse = await api.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "planner",
          targetType: "issue",
          targetId: "issue:acme/looper:124",
          repo: "acme/looper",
        }),
      }),
    );
    const createGenericPlannerBody =
      (await createGenericPlannerResponse.json()) as {
        data: {
          type: string;
          targetType: string;
          targetId: string;
          repo: string;
        };
      };
    expect(createGenericPlannerResponse.status).toBe(200);
    expect(createGenericPlannerBody.data.type).toBe("planner");
    expect(createGenericPlannerBody.data.targetType).toBe("issue");
    expect(createGenericPlannerBody.data.targetId).toBe(
      "issue:acme/looper:124",
    );
    expect(createGenericPlannerBody.data.repo).toBe("acme/looper");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("supports project add route", async () => {
    const { store, rootDir } = await createFixture();
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects`,
      ),
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
        }) => ({
          project: {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
            createdAt: "2026-04-11T12:00:00.000Z",
            updatedAt: "2026-04-11T12:00:00.000Z",
          },
          repo: "powerformer/looper",
          discoveredPullRequests: 1,
          discoveredWorktrees: 2,
          warnings: [],
        }),
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const response = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/looper",
          id: "looper",
          name: "Looper",
        }),
      }),
    );
    const body = (await response.json()) as {
      data: { id: string; repo: string; discoveredPullRequests: number };
    };

    expect(response.status).toBe(200);
    expect(body.data.id).toBe("looper");
    expect(body.data.repo).toBe("powerformer/looper");
    expect(body.data.discoveredPullRequests).toBe(1);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("rejects unsafe project ids through the API", async () => {
    const { store, rootDir } = await createFixture();
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects-invalid-id`,
      ),
      store,
      projects: {
        addProject: async () => {
          throw new InvalidProjectIdError("../../tmp");
        },
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const response = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/looper",
          id: "../../tmp",
          name: "Looper",
        }),
      }),
    );
    const body = (await response.json()) as {
      error: { code: string; message: string };
    };

    expect(response.status).toBe(400);
    expect(body.error.code).toBe("VALIDATION_FAILED");
    expect(body.error.message).toContain('Invalid project id "../../tmp"');

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("rejects reserved legacy-id project ids through the API", async () => {
    const { store, rootDir } = await createFixture();
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects-invalid-legacy-id`,
      ),
      store,
      projects: {
        addProject: async () => {
          throw new InvalidProjectIdError("legacy-id-Li4vdG1w");
        },
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const response = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/looper",
          id: "legacy-id-Li4vdG1w",
          name: "Looper",
        }),
      }),
    );
    const body = (await response.json()) as {
      error: { code: string; message: string };
    };

    expect(response.status).toBe(400);
    expect(body.error.code).toBe("VALIDATION_FAILED");
    expect(body.error.message).toContain(
      'Invalid project id "legacy-id-Li4vdG1w"',
    );

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("derives legacy-looking project ids without pre-normalizing through the API", async () => {
    const { store, rootDir } = await createFixture();
    let derivedId: string | undefined;
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects-derived-id`,
      ),
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
          idSource?: "explicit" | "derived";
        }) => {
          derivedId = input.id;
          const project = {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: null, worktreeRoot: null }),
            createdAt: "2026-04-11T12:00:00.000Z",
            updatedAt: "2026-04-11T12:00:00.000Z",
          };
          store.projects.upsert(project);
          return {
            project,
            repo: null,
            discoveredPullRequests: 0,
            discoveredWorktrees: 0,
            warnings: [],
          };
        },
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const response = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/legacy-id-example",
          name: "Looper",
        }),
      }),
    );
    const body = (await response.json()) as {
      data: { id: string; name: string; repoPath: string };
    };

    expect(response.status).toBe(200);
    expect(derivedId).toBe("legacy-id-example");
    expect(body.data.id).toBe("legacy-id-example");
    expect(body.data.name).toBe("Looper");
    expect(body.data.repoPath).toBe("/tmp/repos/legacy-id-example");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("derives project ids with the same mixed-separator parser used by project normalization", async () => {
    const { store, rootDir } = await createFixture();
    let derivedId = "";
    let idSource: "explicit" | "derived" | undefined;
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects-mixed-separators`,
      ),
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
          idSource?: "explicit" | "derived";
        }) => {
          derivedId = input.id;
          idSource = input.idSource;
          const project = {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: null, worktreeRoot: null }),
            createdAt: "2026-04-11T12:00:00.000Z",
            updatedAt: "2026-04-11T12:00:00.000Z",
          };
          store.projects.upsert(project);
          return {
            project,
            repo: null,
            discoveredPullRequests: 0,
            discoveredWorktrees: 0,
            warnings: [],
          };
        },
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const response = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos\\legacy-id-example",
          name: "Looper",
        }),
      }),
    );
    const body = (await response.json()) as {
      data: { id: string; name: string; repoPath: string };
    };

    expect(response.status).toBe(200);
    expect(derivedId).toBe("legacy-id-example");
    expect(idSource).toBe("derived");
    expect(body.data.id).toBe("legacy-id-example");
    expect(body.data.repoPath).toBe("/tmp/repos\\legacy-id-example");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("keeps derived legacy-id project ids distinct from normalized repo names", async () => {
    const { store, rootDir } = await createFixture();
    const apiWithProjects = createLooperdApi({
      config: createDefaultLooperConfig(rootDir),
      logger: await createLogger(
        createDefaultLooperConfig(rootDir).logging,
        `${rootDir}/logs-projects-distinct-derived-id`,
      ),
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
        }) => {
          const project = {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: null, worktreeRoot: null }),
            createdAt: "2026-04-11T12:00:00.000Z",
            updatedAt: "2026-04-11T12:00:00.000Z",
          };
          store.projects.upsert(project);
          return {
            project,
            repo: null,
            discoveredPullRequests: 0,
            discoveredWorktrees: 0,
            warnings: [],
          };
        },
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const legacyResponse = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/legacy-id-foo",
          name: "Legacy repo",
        }),
      }),
    );
    const legacyBody = (await legacyResponse.json()) as {
      data: { id: string; repoPath: string };
    };

    const normalizedResponse = await apiWithProjects.handle(
      new Request("http://localhost/api/v1/projects", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          repoPath: "/tmp/repos/project-legacy-id-foo",
          name: "Normalized repo",
        }),
      }),
    );
    const normalizedBody = (await normalizedResponse.json()) as {
      data: { id: string; repoPath: string };
    };

    expect(legacyResponse.status).toBe(200);
    expect(normalizedResponse.status).toBe(200);
    expect(legacyBody.data.id).toBe("legacy-id-foo");
    expect(normalizedBody.data.id).toBe("project-legacy-id-foo");
    expect(store.projects.getById("legacy-id-foo")?.repoPath).toBe(
      "/tmp/repos/legacy-id-foo",
    );
    expect(store.projects.getById("project-legacy-id-foo")?.repoPath).toBe(
      "/tmp/repos/project-legacy-id-foo",
    );

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("rejects reviewer/fixer create and start when no coding agent is configured", async () => {
    const { store, rootDir } = await createFixture();
    store.loops.upsert({
      id: "loop_fixer_no_agent",
      seq: 4,
      projectId: "project_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:99",
      repo: "acme/looper",
      prNumber: 99,
      status: "paused",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: null,
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
    });

    const configWithoutAgent = createDefaultLooperConfig(rootDir);
    configWithoutAgent.agent.vendor = undefined;
    const apiWithoutAgent = createLooperdApi({
      config: configWithoutAgent,
      logger: await createLogger(
        configWithoutAgent.logging,
        `${rootDir}/logs-no-agent`,
      ),
      store,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 0 }),
    });

    const createFixerResponse = await apiWithoutAgent.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "fixer",
          targetType: "pull_request",
          repo: "acme/looper",
          prNumber: 88,
        }),
      }),
    );
    const createFixerBody = (await createFixerResponse.json()) as {
      error: { code: string; message: string };
    };
    expect(createFixerResponse.status).toBe(400);
    expect(createFixerBody.error.code).toBe("AGENT_NOT_CONFIGURED");

    const startFixerResponse = await apiWithoutAgent.handle(
      new Request("http://localhost/api/v1/loops/loop_fixer_no_agent/start", {
        method: "POST",
      }),
    );
    const startFixerBody = (await startFixerResponse.json()) as {
      error: { code: string; message: string };
    };
    expect(startFixerResponse.status).toBe(400);
    expect(startFixerBody.error.code).toBe("AGENT_NOT_CONFIGURED");

    const createWorkerResponse = await apiWithoutAgent.handle(
      new Request("http://localhost/api/v1/loops", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          type: "worker",
          targetType: "project",
          targetId: "project_1",
        }),
      }),
    );
    expect(createWorkerResponse.status).toBe(200);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("returns an empty active-runs list when no runs are running", async () => {
    const { api, store, rootDir } = await createFixture();

    const existingRun = store.runs.getById("run_1");
    if (!existingRun) {
      throw new Error("expected run_1 to exist");
    }
    store.runs.upsert({
      ...existingRun,
      status: "completed",
      endedAt: "2026-04-11T12:10:00.000Z",
      updatedAt: "2026-04-11T12:10:00.000Z",
    });

    const response = await api.handle(
      new Request("http://localhost/api/v1/runs/active"),
    );
    const body = (await response.json()) as {
      data: { items: Array<Record<string, unknown>> };
    };

    expect(response.status).toBe(200);
    expect(body.data.items).toEqual([]);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("returns active runs with target/agent shapes, collapse rules, and filters", async () => {
    const { api, store, rootDir } = await createFixture();

    store.loops.upsert({
      id: "loop_worker_1",
      seq: 5,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: "2026-04-11T12:01:00.000Z",
      nextRunAt: "2026-04-11T12:01:00.000Z",
      createdAt: "2026-04-11T12:01:00.000Z",
      updatedAt: "2026-04-11T12:01:00.000Z",
    });
    store.runs.upsert({
      id: "run_worker_1",
      loopId: "loop_worker_1",
      status: "running",
      currentStep: "execute",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:01:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:01:30.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:01:00.000Z",
      updatedAt: "2026-04-11T12:01:30.000Z",
    });

    store.loops.upsert({
      id: "loop_fixer_1",
      seq: 6,
      projectId: "project_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:43",
      repo: "acme/looper",
      prNumber: 43,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: "2026-04-11T12:02:00.000Z",
      nextRunAt: "2026-04-11T12:02:00.000Z",
      createdAt: "2026-04-11T12:02:00.000Z",
      updatedAt: "2026-04-11T12:02:00.000Z",
    });
    store.runs.upsert({
      id: "run_fixer_1",
      loopId: "loop_fixer_1",
      status: "running",
      currentStep: "fix",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:02:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:02:30.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:02:00.000Z",
      updatedAt: "2026-04-11T12:02:30.000Z",
    });

    store.loops.upsert({
      id: "loop_planner_1",
      seq: 7,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:77",
      repo: "acme/looper",
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: "2026-04-11T12:02:45.000Z",
      nextRunAt: "2026-04-11T12:02:45.000Z",
      createdAt: "2026-04-11T12:02:45.000Z",
      updatedAt: "2026-04-11T12:02:45.000Z",
    });
    store.runs.upsert({
      id: "run_planner_1",
      loopId: "loop_planner_1",
      status: "running",
      currentStep: "plan",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:02:45.000Z",
      lastHeartbeatAt: "2026-04-11T12:02:50.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:02:45.000Z",
      updatedAt: "2026-04-11T12:02:50.000Z",
    });

    // fallback label should use project id when project metadata is unavailable
    store.loops.upsert({
      id: "loop_worker_fallback",
      seq: 8,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_fallback",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: "2026-04-11T12:03:00.000Z",
      nextRunAt: "2026-04-11T12:03:00.000Z",
      createdAt: "2026-04-11T12:03:00.000Z",
      updatedAt: "2026-04-11T12:03:00.000Z",
    });
    store.runs.upsert({
      id: "run_worker_fallback",
      loopId: "loop_worker_fallback",
      status: "running",
      currentStep: "plan",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:03:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:03:30.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:03:00.000Z",
      updatedAt: "2026-04-11T12:03:30.000Z",
    });

    store.loops.upsert({
      id: "loop_worker_queued",
      seq: 9,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: "2026-04-11T12:04:00.000Z",
      createdAt: "2026-04-11T12:04:00.000Z",
      updatedAt: "2026-04-11T12:04:00.000Z",
    });
    store.queue.upsert({
      id: "queue_worker_queued",
      projectId: "project_1",
      loopId: "loop_worker_queued",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      dedupeKey: "worker:loop_worker_queued",
      priority: 3,
      status: "running",
      availableAt: "2026-04-11T12:04:00.000Z",
      attempts: 0,
      maxAttempts: 3,
      claimedBy: "executor_queued",
      claimedAt: "2026-04-11T12:04:01.000Z",
      startedAt: "2026-04-11T12:04:01.000Z",
      finishedAt: null,
      lockKey: "worker:loop_worker_queued",
      payloadJson: JSON.stringify({ repo: "acme/looper", baseBranch: "main" }),
      lastError: null,
      lastErrorKind: null,
      createdAt: "2026-04-11T12:04:00.000Z",
      updatedAt: "2026-04-11T12:04:01.000Z",
    });

    // stale queued loops without queued scheduler work should not be shown
    store.loops.upsert({
      id: "loop_worker_stale_queued",
      seq: 10,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: "2026-04-11T12:05:00.000Z",
      createdAt: "2026-04-11T12:05:00.000Z",
      updatedAt: "2026-04-11T12:05:00.000Z",
    });

    // null-runId active execution is ignored
    store.agentExecutions.upsert({
      id: "agent_exec_null_run",
      projectId: "project_1",
      loopId: "loop_worker_1",
      runId: null,
      vendor: "opencode",
      status: "running",
      pid: 99901,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 1,
      lastHeartbeatAt: "2026-04-11T12:02:00.000Z",
      outputJson: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:02:00.000Z",
      endedAt: null,
      metadataJson: null,
      createdAt: "2026-04-11T12:02:00.000Z",
      updatedAt: "2026-04-11T12:02:00.000Z",
    });

    // duplicate active executions collapse to one agent summary with activeCount
    store.agentExecutions.upsert({
      id: "agent_exec_worker_old",
      projectId: "project_1",
      loopId: "loop_worker_1",
      runId: "run_worker_1",
      vendor: "opencode",
      status: "running",
      pid: 11111,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 2,
      lastHeartbeatAt: "2026-04-11T12:01:40.000Z",
      outputJson: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:01:10.000Z",
      endedAt: null,
      metadataJson: null,
      createdAt: "2026-04-11T12:01:10.000Z",
      updatedAt: "2026-04-11T12:01:40.000Z",
    });
    store.agentExecutions.upsert({
      id: "agent_exec_worker_new",
      projectId: "project_1",
      loopId: "loop_worker_1",
      runId: "run_worker_1",
      vendor: "opencode",
      status: "running",
      pid: 22222,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 5,
      lastHeartbeatAt: "2026-04-11T12:01:50.000Z",
      outputJson: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:01:20.000Z",
      endedAt: null,
      metadataJson: null,
      createdAt: "2026-04-11T12:01:20.000Z",
      updatedAt: "2026-04-11T12:01:50.000Z",
    });
    store.agentExecutions.upsert({
      id: "agent_exec_fixer",
      projectId: "project_1",
      loopId: "loop_fixer_1",
      runId: "run_fixer_1",
      vendor: "opencode",
      status: "running",
      pid: 33333,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 3,
      lastHeartbeatAt: "2026-04-11T12:02:40.000Z",
      outputJson: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:02:10.000Z",
      endedAt: null,
      metadataJson: null,
      createdAt: "2026-04-11T12:02:10.000Z",
      updatedAt: "2026-04-11T12:02:40.000Z",
    });

    const response = await api.handle(
      new Request("http://localhost/api/v1/runs/active"),
    );
    const body = (await response.json()) as {
      data: {
        items: Array<{
          runId: string | null;
          type: string;
          currentStep: string | null;
          status: string;
          target: {
            type: string;
            label: string;
            projectId?: string;
            issueNumber?: number;
          };
          agent: {
            executionId: string;
            activeCount: number;
            pid: number | null;
          } | null;
        }>;
      };
    };

    expect(response.status).toBe(200);
    expect(body.data.items.map((item) => item.runId)).toEqual([
      "run_worker_1",
      "run_fixer_1",
      "run_1",
      "run_planner_1",
      "run_worker_fallback",
      null,
    ]);

    const reviewer = body.data.items.find((item) => item.runId === "run_1");
    expect(reviewer).toMatchObject({
      type: "reviewer",
      currentStep: "review",
      target: {
        type: "pull_request",
        label: "acme/looper#42",
      },
      agent: null,
    });

    const worker = body.data.items.find(
      (item) => item.runId === "run_worker_1",
    );
    expect(worker).toMatchObject({
      type: "worker",
      currentStep: "execute",
      target: {
        type: "project",
        projectId: "project_1",
        label: "Looper",
      },
      agent: {
        executionId: "agent_exec_worker_new",
        activeCount: 2,
        pid: 22222,
      },
    });

    const fixer = body.data.items.find((item) => item.runId === "run_fixer_1");
    expect(fixer).toMatchObject({
      type: "fixer",
      currentStep: "fix",
      target: {
        type: "pull_request",
        label: "acme/looper#43",
      },
      agent: {
        executionId: "agent_exec_fixer",
        activeCount: 1,
        pid: 33333,
      },
    });

    const planner = body.data.items.find(
      (item) => item.runId === "run_planner_1",
    );
    expect(planner).toMatchObject({
      type: "planner",
      currentStep: "plan",
      target: {
        type: "issue",
        issueNumber: 77,
        label: "acme/looper#77",
      },
      agent: null,
    });

    const fallbackTaskTarget = body.data.items.find(
      (item) => item.runId === "run_worker_fallback",
    );
    expect(fallbackTaskTarget?.target).toMatchObject({
      type: "project",
      projectId: "project_fallback",
      label: "project_fallback",
    });

    const queuedWorker = body.data.items.find((item) => item.runId === null);
    expect(queuedWorker).toMatchObject({
      loopId: "loop_worker_queued",
      type: "worker",
      status: "queued",
      currentStep: null,
      target: {
        type: "project",
        projectId: "project_1",
        label: "Looper",
      },
      agent: null,
    });

    const typeFiltered = await api.handle(
      new Request("http://localhost/api/v1/runs/active?type=worker"),
    );
    const typeFilteredBody = (await typeFiltered.json()) as {
      data: { items: Array<{ runId: string | null }> };
    };
    expect(typeFilteredBody.data.items.map((item) => item.runId)).toEqual([
      "run_worker_1",
      "run_worker_fallback",
      null,
    ]);

    const projectFiltered = await api.handle(
      new Request("http://localhost/api/v1/runs/active?projectId=project_1"),
    );
    const projectFilteredBody = (await projectFiltered.json()) as {
      data: { items: Array<{ runId: string | null }> };
    };
    expect(projectFilteredBody.data.items).toHaveLength(6);

    const prFiltered = await api.handle(
      new Request(
        "http://localhost/api/v1/runs/active?repo=acme%2Flooper&prNumber=43",
      ),
    );
    const prFilteredBody = (await prFiltered.json()) as {
      data: { items: Array<{ runId: string }> };
    };
    expect(prFilteredBody.data.items.map((item) => item.runId)).toEqual([
      "run_fixer_1",
    ]);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("resolves loop routes by seq and returns logs envelopes", async () => {
    const { api, store, rootDir } = await createFixture();

    store.runs.upsert({
      id: "run_2",
      loopId: "loop_1",
      status: "completed",
      currentStep: null,
      lastCompletedStep: "execute",
      checkpointJson: null,
      summary: "latest run",
      errorMessage: null,
      startedAt: "2026-04-11T12:00:15.000Z",
      lastHeartbeatAt: "2026-04-11T12:00:20.000Z",
      endedAt: "2026-04-11T12:00:20.000Z",
      createdAt: "2026-04-11T12:00:15.000Z",
      updatedAt: "2026-04-11T12:00:20.000Z",
    });

    store.agentExecutions.upsert({
      id: "agent_exec_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      vendor: "opencode",
      status: "running",
      pid: 777,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 2,
      lastHeartbeatAt: "2026-04-11T12:00:10.000Z",
      outputJson: JSON.stringify({ stdout: "out1\nout2\n", stderr: "err1\n" }),
      errorMessage: null,
      startedAt: "2026-04-11T12:00:05.000Z",
      endedAt: null,
      metadataJson: null,
      createdAt: "2026-04-11T12:00:05.000Z",
      updatedAt: "2026-04-11T12:00:10.000Z",
    });
    store.agentExecutions.upsert({
      id: "agent_exec_2",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_2",
      vendor: "opencode",
      status: "completed",
      pid: 778,
      commandJson: "{}",
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 2,
      lastHeartbeatAt: "2026-04-11T12:00:20.000Z",
      outputJson: JSON.stringify({
        stdout: "latest stdout\n",
        stderr: " latest stderr\n",
      }),
      errorMessage: null,
      startedAt: "2026-04-11T12:00:15.000Z",
      endedAt: "2026-04-11T12:00:20.000Z",
      metadataJson: null,
      createdAt: "2026-04-11T12:00:15.000Z",
      updatedAt: "2026-04-11T12:00:20.000Z",
    });

    store.loops.upsert({
      id: "loop_2",
      seq: 2,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "paused",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: null,
      createdAt: "2026-04-11T12:01:00.000Z",
      updatedAt: "2026-04-11T12:01:00.000Z",
    });

    const bySeqResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/1"),
    );
    const bySeqBody = (await bySeqResponse.json()) as {
      data: { id: string; seq: number };
    };
    expect(bySeqResponse.status).toBe(200);
    expect(bySeqBody.data.id).toBe("loop_1");
    expect(bySeqBody.data.seq).toBe(1);

    const logsWithAgentResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/1/logs"),
    );
    const logsWithAgentBody = (await logsWithAgentResponse.json()) as {
      data: {
        seq: number;
        loopId: string;
        agent: { stdout: string; stderr: string } | null;
      };
    };
    expect(logsWithAgentBody.data.seq).toBe(1);
    expect(logsWithAgentBody.data.loopId).toBe("loop_1");
    expect(logsWithAgentBody.data.agent).toMatchObject({
      executionId: "agent_exec_2",
      stdout: "latest stdout",
      stderr: "latest stderr",
    });

    const logsWithoutAgentResponse = await api.handle(
      new Request("http://localhost/api/v1/loops/2/logs"),
    );
    const logsWithoutAgentBody = (await logsWithoutAgentResponse.json()) as {
      data: {
        seq: number;
        run: Record<string, unknown> | null;
        agent: unknown;
      };
    };
    expect(logsWithoutAgentBody.data.seq).toBe(2);
    expect(logsWithoutAgentBody.data.run).toBeNull();
    expect(logsWithoutAgentBody.data.agent).toBeNull();

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("returns active run detail by seq with worktree and supports stop by seq", async () => {
    const stopCalls: Array<{ loopId: string; reason: string }> = [];
    const { api, store, rootDir } = await createFixture({
      runtimeControl: {
        stopLoop: async (input) => {
          stopCalls.push(input);
          return {
            loopId: input.loopId,
            runId: "run_1",
            executionId: "agent_exec_1",
            vendor: "opencode",
            pid: 777,
            stopped: true,
          };
        },
        triggerSchedulerTick: () => {},
      },
    });

    const existingRun = store.runs.getById("run_1");
    if (!existingRun) {
      throw new Error("expected run_1 to exist");
    }
    store.runs.upsert({
      ...existingRun,
      checkpointJson: JSON.stringify({
        worktree: {
          id: "wt_1",
          path: "/tmp/worktrees/loop-1",
          branch: "feature/loop-1",
        },
      }),
      updatedAt: "2026-04-11T12:00:30.000Z",
    });

    const activeRunsResponse = await api.handle(
      new Request("http://localhost/api/v1/runs/active"),
    );
    const activeRunsBody = (await activeRunsResponse.json()) as {
      data: {
        items: Array<{ seq: number; worktree: { path: string } | null }>;
      };
    };
    expect(activeRunsBody.data.items[0]).toMatchObject({
      seq: 1,
      worktree: { path: "/tmp/worktrees/loop-1" },
    });

    const detailResponse = await api.handle(
      new Request("http://localhost/api/v1/runs/active/1"),
    );
    const detailBody = (await detailResponse.json()) as {
      data: {
        loopId: string;
        seq: number;
        worktree: { branch: string } | null;
      };
    };
    expect(detailBody.data.loopId).toBe("loop_1");
    expect(detailBody.data.seq).toBe(1);
    expect(detailBody.data.worktree).toMatchObject({
      branch: "feature/loop-1",
    });

    const stopResponse = await api.handle(
      new Request("http://localhost/api/v1/runs/active/1/stop", {
        method: "POST",
      }),
    );
    const stopBody = (await stopResponse.json()) as {
      data: { loopId: string; stopped: boolean };
    };
    expect(stopResponse.status).toBe(200);
    expect(stopBody.data.loopId).toBe("loop_1");
    expect(stopBody.data.stopped).toBe(true);
    expect(stopCalls).toEqual([
      {
        loopId: "loop_1",
        reason: "Stopped by user via selector 1",
      },
    ]);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("matches the machine-verifiable daemon HTTP contract routes and headers", async () => {
    const contract = await loadDaemonHttpContractArtifact();
    const requestId = "contract-request-id";
    const runtimeStopCalls: Array<{ loopId: string; reason: string }> = [];
    const { api, config, logger, store, rootDir } = await createFixture({
      runtimeControl: {
        async stopLoop(input) {
          runtimeStopCalls.push(input);
          return {
            stopped: true,
            loopId: input.loopId,
          };
        },
        triggerSchedulerTick() {},
      },
    });

    const projectsApi = createLooperdApi({
      config,
      logger,
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
        }) => ({
          project: {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
            createdAt: "2026-04-11T12:00:00.000Z",
            updatedAt: "2026-04-11T12:00:00.000Z",
          },
          repo: "powerformer/looper",
          discoveredPullRequests: 1,
          discoveredWorktrees: 2,
          warnings: [],
        }),
      } as never,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
      runtimeControl: {
        async stopLoop(input) {
          runtimeStopCalls.push(input);
          return {
            stopped: true,
            loopId: input.loopId,
          };
        },
        triggerSchedulerTick() {},
      },
    });

    for (const route of contract.routes) {
      const targetApi =
        route.fixture === "projects-enabled" ? projectsApi : api;
      const headers = new Headers(route.request?.headers ?? {});
      headers.set(contract.sharedBehavior.requestIdHeader.name, requestId);

      const response = await targetApi.handle(
        new Request(`http://localhost${route.path}`, {
          method: route.method,
          headers,
          body: route.request?.body
            ? JSON.stringify(route.request.body)
            : undefined,
        }),
      );
      const body = (await response.json()) as { requestId: string };

      expect(response.status, route.id).toBe(route.expectedStatus);
      expect(response.headers.get("content-type"), route.id).toBe(
        contract.sharedBehavior.responseHeaders["content-type"] ?? null,
      );
      expect(body.requestId, route.id).toBe(requestId);
    }

    expect(runtimeStopCalls).toEqual([
      {
        loopId: "loop_1",
        reason: "Stopped by user via selector 1",
      },
    ]);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("matches the machine-verifiable daemon HTTP contract auth behavior", async () => {
    const contract = await loadDaemonHttpContractArtifact();
    const { config, logger, store, rootDir } = await createFixture();

    config.server.authMode = "local-token";

    const misconfiguredApi = createLooperdApi({
      config,
      logger,
      store,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });
    const misconfiguredResponse = await misconfiguredApi.handle(
      new Request("http://localhost/api/v1/status"),
    );
    expect(misconfiguredResponse.status).toBe(
      contract.sharedBehavior.auth.localTokenMode.missingConfiguredToken.status,
    );

    config.server.localToken = "secret-token";
    const authedApi = createLooperdApi({
      config,
      logger,
      store,
      getStartedAt: () => new Date("2026-04-11T12:00:00.000Z"),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
    });

    const missingTokenResponse = await authedApi.handle(
      new Request("http://localhost/api/v1/status"),
    );
    expect(missingTokenResponse.status).toBe(
      contract.sharedBehavior.auth.localTokenMode.missingOrWrongBearerToken
        .status,
    );

    const wrongTokenResponse = await authedApi.handle(
      new Request("http://localhost/api/v1/status", {
        headers: { authorization: "Bearer wrong-token" },
      }),
    );
    expect(wrongTokenResponse.status).toBe(
      contract.sharedBehavior.auth.localTokenMode.missingOrWrongBearerToken
        .status,
    );

    const okResponse = await authedApi.handle(
      new Request("http://localhost/api/v1/status", {
        headers: {
          authorization: "Bearer secret-token",
          "x-request-id": "auth-ok-request-id",
        },
      }),
    );
    const okBody = (await okResponse.json()) as { requestId: string };
    expect(okResponse.status).toBe(
      contract.sharedBehavior.auth.localTokenMode.matchingBearerToken.status,
    );
    expect(okResponse.headers.get("content-type")).toBe(
      contract.sharedBehavior.responseHeaders["content-type"] ?? null,
    );
    expect(okBody.requestId).toBe("auth-ok-request-id");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("matches the machine-verifiable daemon HTTP contract generic 404 and 405 behavior", async () => {
    const contract = await loadDaemonHttpContractArtifact();
    const { api, store, rootDir } = await createFixture();

    const seenPaths = new Set<string>();
    for (const route of contract.routes) {
      if (seenPaths.has(route.path)) {
        continue;
      }
      seenPaths.add(route.path);

      const response = await api.handle(
        new Request(`http://localhost${route.path}`, {
          method: "DELETE",
        }),
      );
      expect(response.status, route.path).toBe(
        contract.sharedBehavior.genericStatuses.methodNotAllowed,
      );
    }

    const missingRouteResponse = await api.handle(
      new Request("http://localhost/api/v1/does-not-exist"),
    );
    expect(missingRouteResponse.status).toBe(
      contract.sharedBehavior.genericStatuses.routeNotFound,
    );

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("matches the machine-verifiable daemon HTTP error fixtures", async () => {
    const errorArtifact = await loadDaemonHttpErrorArtifact();

    for (const errorCase of errorArtifact.cases) {
      const fixture = await createFixture();

      try {
        let api = fixture.api;

        switch (errorCase.fixture) {
          case "auth-misconfigured": {
            fixture.config.server.authMode = "local-token";
            fixture.config.server.localToken = "";
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "auth-required": {
            fixture.config.server.authMode = "local-token";
            fixture.config.server.localToken = "secret-token";
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "runtime-control-disabled": {
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "inactive-run": {
            const run = fixture.store.runs.getLatestByLoopId("loop_1");
            if (!run) {
              throw new Error("expected seeded run_1 for inactive-run fixture");
            }

            fixture.store.runs.upsert({
              ...run,
              status: "success",
              endedAt: SEEDED_NOW,
              updatedAt: SEEDED_NOW,
            });
            break;
          }
          case "ambiguous-project-repo": {
            fixture.store.projects.upsert({
              id: "project_2",
              name: "Looper Mirror",
              repoPath: "/tmp/looper-mirror",
              baseBranch: "main",
              archived: false,
              metadataJson: JSON.stringify({ repo: "acme/looper" }),
              createdAt: SEEDED_NOW,
              updatedAt: SEEDED_NOW,
            });
            break;
          }
          case "pull-request-mismatch": {
            fixture.store.projects.upsert({
              id: "project_2",
              name: "Other Repo",
              repoPath: "/tmp/other-repo",
              baseBranch: "main",
              archived: false,
              metadataJson: JSON.stringify({ repo: "acme/other" }),
              createdAt: SEEDED_NOW,
              updatedAt: SEEDED_NOW,
            });
            break;
          }
          case "projects-id-conflict": {
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              projects: {
                addProject: async (input: {
                  id: string;
                  name: string;
                  repoPath: string;
                  baseBranch: string;
                }) => {
                  throw new ProjectIdCollisionError(input.id);
                },
              } as never,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "projects-internal-error": {
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              projects: {
                addProject: async () => {
                  throw new Error("boom");
                },
              } as never,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "projects-invalid-id": {
            api = createLooperdApi({
              config: fixture.config,
              logger: fixture.logger,
              store: fixture.store,
              projects: {
                addProject: async () => {
                  throw new InvalidProjectIdError("../../tmp");
                },
              } as never,
              getStartedAt: () => new Date(SEEDED_NOW),
              getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
            });
            break;
          }
          case "default":
            if (errorCase.id === "agent-not-configured") {
              fixture.config.agent.vendor = undefined;
              api = createLooperdApi({
                config: fixture.config,
                logger: fixture.logger,
                store: fixture.store,
                getStartedAt: () => new Date(SEEDED_NOW),
                getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
              });
            }
            break;
        }

        const headers = new Headers(errorCase.request?.headers ?? {});
        const response = await api.handle(
          new Request(`http://localhost${errorCase.path}`, {
            method: errorCase.method,
            headers,
            body: errorCase.request?.body
              ? JSON.stringify(errorCase.request.body)
              : undefined,
          }),
        );
        const body = await response.json();

        expect(response.status, errorCase.id).toBe(errorCase.expectedStatus);
        expect(response.headers.get("content-type"), errorCase.id).toBe(
          errorArtifact.sharedBehavior.responseHeaders["content-type"] ?? null,
        );
        expect(
          normalizeResponseFixture(body, { rootDir: fixture.rootDir }),
          errorCase.id,
        ).toEqual(errorCase.body);
      } finally {
        fixture.store.close();
        await rm(fixture.rootDir, { recursive: true, force: true });
      }
    }
  });

  test("matches the machine-verifiable daemon HTTP response fixtures", async () => {
    const contract = await loadDaemonHttpContractArtifact();
    const responseArtifact = await loadDaemonHttpResponseArtifact();
    const fixturesByRouteId = new Map(
      responseArtifact.routes.map((route) => [route.id, route]),
    );
    const { api, config, logger, store, rootDir } = await createFixture({
      runtimeControl: {
        async stopLoop(input) {
          return {
            stopped: true,
            loopId: input.loopId,
          };
        },
        triggerSchedulerTick() {},
      },
    });

    const projectsApi = createLooperdApi({
      config,
      logger,
      store,
      projects: {
        addProject: async (input: {
          id: string;
          name: string;
          repoPath: string;
          baseBranch: string;
        }) => ({
          project: {
            id: input.id,
            name: input.name,
            repoPath: input.repoPath,
            baseBranch: input.baseBranch,
            archived: false,
            metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
            createdAt: SEEDED_NOW,
            updatedAt: SEEDED_NOW,
          },
          repo: "powerformer/looper",
          discoveredPullRequests: 1,
          discoveredWorktrees: 2,
          warnings: [],
        }),
      } as never,
      getStartedAt: () => new Date(SEEDED_NOW),
      getRecoverySummary: () => ({ expiredLocksReleased: 1 }),
      runtimeControl: {
        async stopLoop(input) {
          return {
            stopped: true,
            loopId: input.loopId,
          };
        },
        triggerSchedulerTick() {},
      },
    });

    for (const route of contract.routes) {
      const expected = fixturesByRouteId.get(route.id);
      expect(expected, route.id).toBeDefined();
      if (!expected) {
        continue;
      }

      const targetApi =
        route.fixture === "projects-enabled" ? projectsApi : api;
      const headers = new Headers(route.request?.headers ?? {});
      headers.set("x-request-id", "fixture-request-id");

      const response = await targetApi.handle(
        new Request(`http://localhost${route.path}`, {
          method: route.method,
          headers,
          body: route.request?.body
            ? JSON.stringify(route.request.body)
            : undefined,
        }),
      );
      const body = await response.json();

      expect(response.status, route.id).toBe(expected.status);
      expect(normalizeResponseFixture(body, { rootDir }), route.id).toEqual(
        expected.body,
      );
    }

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("matches the machine-verifiable daemon HTTP request body fixtures", async () => {
    const contract = await loadDaemonHttpContractArtifact();
    const requestArtifact = await loadDaemonHttpRequestArtifact();
    const contractRoutesWithJsonBodies = contract.routes.filter(
      (route) => route.request?.body,
    );

    expect(requestArtifact.routes.map((route) => route.id)).toEqual(
      contractRoutesWithJsonBodies.map((route) => route.id),
    );

    for (const route of requestArtifact.routes) {
      const contractRoute = contract.routes.find(
        (candidate) => candidate.id === route.id,
      );

      expect(contractRoute, route.id).toBeDefined();
      if (!contractRoute) {
        continue;
      }

      expect(contractRoute.request, route.id).toBeDefined();
      if (!contractRoute.request) {
        continue;
      }

      expect(route.method, route.id).toBe(contractRoute.method);
      expect(route.path, route.id).toBe(contractRoute.path);
      expect(route.request, route.id).toEqual(contractRoute.request);
    }
  });
});
