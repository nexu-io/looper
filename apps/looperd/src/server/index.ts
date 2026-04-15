import { randomUUID } from "node:crypto";

import type { Logger } from "../bootstrap/logger";
import {
  InvalidProjectIdError,
  type LooperConfig,
  deriveProjectIdFromRepoPath,
} from "../config/index";
import {
  assertUniqueActiveLoop,
  createLoop,
  createPrLockKey,
  defineIssueLoopTarget,
  defineProjectLoopTarget,
  definePullRequestLoopTarget,
} from "../domain/index";
import {
  ProjectIdCollisionError,
  type ProjectManager,
} from "../projects/index";
import { SchedulerQueue } from "../scheduler/index";
import type { Store } from "../storage/store";
import type {
  AgentExecutionRecord,
  EventLogRecord,
  LoopRecord,
  PullRequestSnapshotRecord,
  RunRecord,
} from "../storage/types";

export interface ApiResponse<T> {
  ok: boolean;
  data?: T;
  error?: {
    code: string;
    message: string;
    details?: unknown;
  };
  requestId: string;
}

export interface LooperdApiContext {
  config: LooperConfig;
  logger: Logger;
  store: Store;
  projects?: ProjectManager;
  runtimeControl?: {
    stopLoop(input: { loopId: string; reason: string }): Promise<{
      stopped: boolean;
      loopId: string;
      runId?: string;
      executionId?: string;
      vendor?: string;
      pid?: number | null;
    }>;
    triggerSchedulerTick(): void;
  };
  getStartedAt(): Date | undefined;
  getRecoverySummary(): Record<string, unknown>;
}

export interface LooperdApi {
  handle(request: Request): Promise<Response>;
}

export interface LooperdApiServer {
  start(): Promise<void>;
  stop(): Promise<void>;
  getPort(): number | undefined;
}

class ApiError extends Error {
  constructor(
    public readonly code: string,
    public readonly status: number,
    message: string,
    public readonly details?: unknown,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function createLooperdApi(context: LooperdApiContext): LooperdApi {
  return {
    async handle(request: Request): Promise<Response> {
      const requestId =
        request.headers.get("x-request-id")?.trim() || randomUUID();

      try {
        authorizeRequest(request, context.config);

        const url = new URL(request.url);
        const pathname = normalizePathname(url.pathname);

        if (!pathname.startsWith("/api/v1")) {
          throw new ApiError(
            "ROUTE_NOT_FOUND",
            404,
            `Unknown route: ${pathname}`,
          );
        }

        let data: unknown;

        switch (true) {
          case pathname === "/api/v1/healthz":
            assertMethod(request, ["GET"], pathname);
            data = buildHealthResponse(context);
            break;
          case pathname === "/api/v1/status":
            assertMethod(request, ["GET"], pathname);
            data = buildStatusResponse(context);
            break;
          case pathname === "/api/v1/config":
            assertMethod(request, ["GET"], pathname);
            data = buildConfigResponse(context.config);
            break;
          case pathname === "/api/v1/events":
            assertMethod(request, ["GET"], pathname);
            data = buildEventsResponse(context, url.searchParams);
            break;
          case pathname.startsWith("/api/v1/events/"):
            assertMethod(request, ["GET"], pathname);
            data = buildEntityEventsResponse(context, pathname);
            break;
          case pathname === "/api/v1/pull-requests":
            assertMethod(request, ["GET"], pathname);
            data = buildPullRequestsResponse(context);
            break;
          case pathname.startsWith("/api/v1/pull-requests/"):
            assertMethod(request, ["GET"], pathname);
            data = buildPullRequestRouteResponse(context, pathname);
            break;
          case pathname === "/api/v1/loops":
            data =
              request.method === "GET"
                ? buildLoopsResponse(context)
                : await buildLoopsCreateResponse(context, request);
            break;
          case pathname === "/api/v1/workers":
            data = await buildWorkersCreateResponse(context, request);
            break;
          case pathname === "/api/v1/planners":
            data = await buildPlannersCreateResponse(context, request);
            break;
          case pathname === "/api/v1/projects":
            data = await buildProjectsRouteResponse(context, request);
            break;
          case pathname.startsWith("/api/v1/loops/"):
            data = await buildLoopRouteResponse(context, request, pathname);
            break;
          case pathname === "/api/v1/runs":
            assertMethod(request, ["GET"], pathname);
            data = buildRunsResponse(context, url.searchParams);
            break;
          case pathname === "/api/v1/runs/active":
            assertMethod(request, ["GET"], pathname);
            data = buildActiveRunsResponse(context, url.searchParams);
            break;
          case pathname.startsWith("/api/v1/runs/active/"):
            data = await buildActiveRunRouteResponse(
              context,
              request,
              pathname,
            );
            break;
          default:
            throw new ApiError(
              "ROUTE_NOT_FOUND",
              404,
              `Unknown route: ${pathname}`,
            );
        }

        return jsonResponse(200, { ok: true, data, requestId });
      } catch (error) {
        const apiError = toApiError(error);
        context.logger.warn("looperd api request failed", {
          requestId,
          method: request.method,
          url: request.url,
          code: apiError.code,
          status: apiError.status,
          message: apiError.message,
        });

        return jsonResponse(apiError.status, {
          ok: false,
          error: {
            code: apiError.code,
            message: apiError.message,
            details: apiError.details,
          },
          requestId,
        });
      }
    },
  };
}

function assertMethod(
  request: Request,
  methods: readonly string[],
  pathname: string,
): void {
  if (methods.includes(request.method)) {
    return;
  }

  throw new ApiError(
    "METHOD_NOT_ALLOWED",
    405,
    `Unsupported method for ${pathname}`,
  );
}

export function createLooperdApiServer(
  context: LooperdApiContext,
): LooperdApiServer {
  const api = createLooperdApi(context);
  let server: Bun.Server<unknown> | undefined;

  return {
    async start(): Promise<void> {
      if (server) {
        return;
      }

      server = Bun.serve({
        hostname: context.config.server.host,
        port: context.config.server.port,
        fetch: (request) => api.handle(request),
        idleTimeout: 30,
      });

      context.logger.info("looperd http server listening", {
        host: server.hostname,
        port: server.port,
      });
    },
    async stop(): Promise<void> {
      if (!server) {
        return;
      }

      server.stop(true);
      server = undefined;
    },
    getPort(): number | undefined {
      return server?.port;
    },
  };
}

function authorizeRequest(request: Request, config: LooperConfig): void {
  if (config.server.authMode !== "local-token") {
    return;
  }

  const authorization = request.headers.get("authorization");
  const expectedToken = config.server.localToken;

  if (!expectedToken) {
    throw new ApiError(
      "AUTH_MISCONFIGURED",
      500,
      "Local token auth is enabled but no token is configured",
    );
  }

  if (authorization !== `Bearer ${expectedToken}`) {
    throw new ApiError("UNAUTHORIZED", 401, "Authorization token is required");
  }
}

function normalizePathname(pathname: string): string {
  return pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
}

function buildHealthResponse(context: LooperdApiContext) {
  const storage = context.store.schema.healthcheck();

  return {
    healthy: storage.ok,
    startedAt: context.getStartedAt()?.toISOString(),
    storage,
  };
}

function buildStatusResponse(context: LooperdApiContext) {
  const loops = context.store.loops.list();
  const runs = context.store.runs.list();
  const queueItems = context.store.queue.list();
  const storage = context.store.schema.healthcheck();

  return {
    service: {
      healthy: storage.ok,
      version: "0.1.0",
      daemonMode: context.config.daemon.mode,
      startedAt: context.getStartedAt()?.toISOString(),
      recovery: context.getRecoverySummary(),
    },
    storage: {
      mode: storage.mode,
      dbPath: storage.dbPath,
      schemaVersion: storage.migration.latestAppliedId ?? "uninitialized",
      pendingMigrations: context.store.schema
        .getMigrationStatus()
        .pending.map((migration) => migration.id),
      healthy: storage.ok,
    },
    scheduler: {
      healthy: true,
      queuedItems: queueItems.filter((item) => item.status === "queued").length,
      runningItems: queueItems.filter((item) => item.status === "running")
        .length,
      completedItems: queueItems.filter((item) => item.status === "completed")
        .length,
      failedItems: queueItems.filter((item) => item.status === "failed").length,
      totalRuns: runs.length,
      activeRuns: runs.filter((run) => run.status === "running").length,
    },
    loops: {
      planner: summarizeLoopType(loops, "planner"),
      reviewer: summarizeLoopType(loops, "reviewer"),
      worker: summarizeLoopType(loops, "worker"),
      fixer: summarizeLoopType(loops, "fixer"),
    },
    safety: {
      allowAutoCommit: context.config.defaults.allowAutoCommit,
      allowAutoPush: context.config.defaults.allowAutoPush,
      allowAutoApprove: context.config.defaults.allowAutoApprove,
      allowRiskyFixes: context.config.defaults.allowRiskyFixes,
      openPrStrategy: context.config.defaults.openPrStrategy ?? "manual",
    },
    notifications: {
      inAppEnabled: context.config.notifications.inApp,
      osascriptEnabled: context.config.notifications.osascript.enabled,
    },
    tools: {
      bun: Boolean(context.config.tools.bunPath),
      git: Boolean(context.config.tools.gitPath),
      gh: Boolean(context.config.tools.ghPath),
      osascript: Boolean(context.config.tools.osascriptPath),
    },
  };
}

function summarizeLoopType(
  loops: ReturnType<Store["loops"]["list"]>,
  type: string,
) {
  const filtered = loops.filter((loop) => loop.type === type);

  return {
    running: filtered.filter((loop) => loop.status === "running").length,
    paused: filtered.filter((loop) => loop.status === "paused").length,
    failed: filtered.filter((loop) => loop.status === "failed").length,
  };
}

function buildConfigResponse(config: LooperConfig) {
  return {
    server: {
      host: config.server.host,
      port: config.server.port,
      baseUrl: config.server.baseUrl,
      authMode: config.server.authMode,
      localTokenConfigured: Boolean(config.server.localToken),
    },
    storage: config.storage,
    scheduler: config.scheduler,
    agent: config.agent,
    logging: config.logging,
    notifications: config.notifications,
    tools: config.tools,
    daemon: config.daemon,
    package: config.package,
    defaults: config.defaults,
    projects: config.projects,
  };
}

function buildEventsResponse(
  context: LooperdApiContext,
  searchParams: URLSearchParams,
) {
  const limitValue = searchParams.get("limit");
  const limit = limitValue ? Number(limitValue) : 100;

  if (!Number.isInteger(limit) || limit <= 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "limit must be a positive integer",
    );
  }

  return {
    items: context.store.events.list(limit).map(serializeEvent),
  };
}

function buildEntityEventsResponse(
  context: LooperdApiContext,
  pathname: string,
) {
  const parts = pathname.split("/").filter(Boolean);
  const entityType = parts[3];
  const entityId = parts[4];

  if (!entityType || !entityId) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "entityType and entityId are required",
    );
  }

  return {
    entityType,
    entityId,
    items: context.store.events
      .listByEntity(
        decodeURIComponent(entityType),
        decodeURIComponent(entityId),
      )
      .map(serializeEvent),
  };
}

function buildPullRequestsResponse(context: LooperdApiContext) {
  const latestSnapshots = dedupeLatestSnapshots(
    context.store.pullRequestSnapshots.list(),
  );
  const loops = context.store.loops.list();
  const identities = collectPullRequestIdentities(latestSnapshots, loops);
  const snapshotByKey = new Map(
    latestSnapshots.map((snapshot) => [
      `${snapshot.repo}#${snapshot.prNumber}`,
      snapshot,
    ]),
  );

  return {
    items: identities.map((identity) =>
      serializePullRequestListItem(
        context,
        identity.repo,
        identity.prNumber,
        snapshotByKey.get(`${identity.repo}#${identity.prNumber}`),
      ),
    ),
  };
}

function buildLoopsResponse(context: LooperdApiContext) {
  return {
    items: context.store.loops.list(),
  };
}

async function buildLoopRouteResponse(
  context: LooperdApiContext,
  request: Request,
  pathname: string,
) {
  const parts = pathname.split("/").filter(Boolean);
  const selector = decodeURIComponent(parts[3] ?? "");
  const subresource = parts[4];

  if (!selector) {
    throw new ApiError("VALIDATION_FAILED", 400, "loopId is required");
  }

  const loop = resolveLoop(context, selector);

  if (!subresource) {
    assertMethod(request, ["GET"], pathname);
    return loop;
  }

  if (subresource === "logs") {
    assertMethod(request, ["GET"], pathname);
    return buildLoopLogsResponse(context, loop);
  }

  if (subresource === "start") {
    assertMethod(request, ["POST"], pathname);
    return mutateLoopStatus(context, loop.id, "running");
  }

  if (subresource === "pause") {
    assertMethod(request, ["POST"], pathname);
    return mutateLoopStatus(context, loop.id, "paused");
  }

  throw new ApiError("ROUTE_NOT_FOUND", 404, `Unknown route: ${pathname}`);
}

function mutateLoopStatus(
  context: LooperdApiContext,
  loopId: string,
  status: string,
) {
  const loop = context.store.loops.getById(loopId);
  if (!loop) {
    throw new ApiError("LOOP_NOT_FOUND", 404, `Loop not found: ${loopId}`);
  }

  if (
    status === "running" &&
    (loop.type === "reviewer" || loop.type === "fixer") &&
    !isCodingAgentConfigured(context.config)
  ) {
    throw new ApiError(
      "AGENT_NOT_CONFIGURED",
      400,
      `Cannot start ${loop.type} loop without config.agent.vendor`,
    );
  }

  const now = new Date().toISOString();
  const updated: typeof loop = {
    ...loop,
    status,
    updatedAt: now,
    ...(status === "running" ? { nextRunAt: now } : {}),
  };
  context.store.loops.upsert(updated);

  return updated;
}

async function buildActiveRunRouteResponse(
  context: LooperdApiContext,
  request: Request,
  pathname: string,
) {
  const parts = pathname.split("/").filter(Boolean);
  const selector = decodeURIComponent(parts[4] ?? "");
  const subresource = parts[5];

  if (!selector) {
    throw new ApiError("VALIDATION_FAILED", 400, "run selector is required");
  }

  const loop = resolveLoop(context, selector);

  if (!subresource) {
    assertMethod(request, ["GET"], pathname);
    return buildActiveRunDetailResponse(context, loop.id);
  }

  if (subresource === "stop") {
    assertMethod(request, ["POST"], pathname);
    if (!context.runtimeControl) {
      throw new ApiError(
        "RUNTIME_CONTROL_UNAVAILABLE",
        501,
        "Runtime control is not available in this process",
      );
    }

    return context.runtimeControl.stopLoop({
      loopId: loop.id,
      reason: `Stopped by user via selector ${selector}`,
    });
  }

  throw new ApiError("ROUTE_NOT_FOUND", 404, `Unknown route: ${pathname}`);
}

function buildActiveRunDetailResponse(
  context: LooperdApiContext,
  loopId: string,
): ActiveRunView {
  const item = buildActiveRunViews(context).find(
    (candidate) => candidate.loopId === loopId,
  );
  if (!item) {
    throw new ApiError(
      "ACTIVE_RUN_NOT_FOUND",
      404,
      `Active run not found for loop: ${loopId}`,
    );
  }

  return item;
}

async function buildWorkersCreateResponse(
  context: LooperdApiContext,
  request: Request,
) {
  assertMethod(request, ["POST"], "/api/v1/workers");

  if (!isCodingAgentConfigured(context.config)) {
    throw new ApiError(
      "AGENT_NOT_CONFIGURED",
      400,
      "Cannot create worker loop without config.agent.vendor",
    );
  }

  const body = await parseJsonBody(request);
  const requestedProjectId = readOptionalString(body, "projectId");
  const requestedRepo = readOptionalString(body, "repo");
  const prompt = readOptionalString(body, "prompt");
  const specPath = readOptionalString(body, "specPath");
  const prNumber = readOptionalPositiveInteger(body, "prNumber");
  const issueNumber = readOptionalPositiveInteger(body, "issueNumber");
  const modeCount =
    Number(Boolean(prNumber)) +
    Number(Boolean(issueNumber)) +
    Number(Boolean(prompt || specPath));
  if (modeCount === 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "prompt or specPath is required unless prNumber or issueNumber is provided",
    );
  }
  if (modeCount > 1) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "worker accepts exactly one input mode: prompt/specPath, prNumber, or issueNumber",
    );
  }

  const project = resolveWorkerProject(context, {
    projectId: requestedProjectId,
    repo: requestedRepo,
    prNumber,
  });
  const projectId = project.id;
  const projectMetadata = parseMetadata(project.metadataJson);
  const repo = requestedRepo ?? readString(projectMetadata.repo);
  const baseBranch =
    readOptionalString(body, "baseBranch") ?? project.baseBranch ?? null;
  if (!repo) {
    throw new ApiError("VALIDATION_FAILED", 400, "repo is required");
  }
  if (!baseBranch) {
    throw new ApiError("VALIDATION_FAILED", 400, "baseBranch is required");
  }

  const pullRequestTarget =
    prNumber == null
      ? null
      : requirePullRequestTarget(context, {
          projectId,
          repo,
          prNumber,
        });
  const planner =
    issueNumber == null
      ? null
      : maybeFindPlannerLoopForIssue(context, {
          projectId,
          repo,
          issueNumber,
        });
  const effectivePrNumber =
    pullRequestTarget?.prNumber ?? planner?.prNumber ?? null;
  const effectiveSpecPath = specPath ?? planner?.specPath ?? null;
  const title =
    readOptionalString(body, "title") ??
    deriveWorkerTitle({
      prompt,
      specPath: effectiveSpecPath,
      repo,
      prNumber: effectivePrNumber,
      issueNumber,
    });
  const now = new Date().toISOString();
  const payload = {
    title,
    prompt,
    specPath: effectiveSpecPath,
    repo,
    baseBranch,
    ...(issueNumber ? { issueNumber } : {}),
    ...(effectivePrNumber ? { prNumber: effectivePrNumber } : {}),
  };
  const loop = createLoopRecord({
    context,
    projectId,
    type: "worker",
    targetType: effectivePrNumber ? "pull_request" : "project",
    targetId: effectivePrNumber ? `pr:${repo}:${effectivePrNumber}` : projectId,
    repo,
    prNumber: effectivePrNumber ?? undefined,
    status: "queued",
    now,
    metadataJson: JSON.stringify({ worker: payload }),
  });

  const scheduler = new SchedulerQueue({
    store: context.store,
    retryMaxAttempts: context.config.scheduler.retryMaxAttempts,
    retryBaseDelayMs: context.config.scheduler.retryBaseDelayMs,
    now: () => new Date(now),
  });
  scheduler.enqueue({
    projectId,
    loopId: loop.id,
    type: "worker",
    targetType: effectivePrNumber ? "pull_request" : "project",
    targetId: effectivePrNumber ? `pr:${repo}:${effectivePrNumber}` : projectId,
    repo,
    ...(effectivePrNumber ? { prNumber: effectivePrNumber } : {}),
    dedupeKey: effectivePrNumber
      ? buildWorkerPullRequestDedupeKey(projectId, repo, effectivePrNumber)
      : `worker:${loop.id}`,
    lockKey: effectivePrNumber
      ? buildWorkerPullRequestLockKey(repo, effectivePrNumber)
      : `worker:${loop.id}`,
    payloadJson: JSON.stringify(payload),
  });

  context.runtimeControl?.triggerSchedulerTick();

  return {
    ...loop,
    ...payload,
    ...(issueNumber ? { issueNumber } : {}),
  };
}

async function buildPlannersCreateResponse(
  context: LooperdApiContext,
  request: Request,
) {
  assertMethod(request, ["POST"], "/api/v1/planners");

  if (!isCodingAgentConfigured(context.config)) {
    throw new ApiError(
      "AGENT_NOT_CONFIGURED",
      400,
      "Cannot create planner loop without config.agent.vendor",
    );
  }

  const body = await parseJsonBody(request);
  const projectId = readRequiredString(body, "projectId");
  const issueNumber = readOptionalPositiveInteger(body, "issueNumber");
  const project = context.store.projects.getById(projectId);
  if (!project) {
    throw new ApiError(
      "PROJECT_NOT_FOUND",
      404,
      `Project not found: ${projectId}`,
    );
  }
  if (!issueNumber) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "issueNumber must be a positive integer",
    );
  }

  const projectMetadata = parseMetadata(project.metadataJson);
  const repo = readString(projectMetadata.repo);
  if (!repo) {
    throw new ApiError("VALIDATION_FAILED", 400, "project repo is required");
  }

  const now = new Date().toISOString();
  const targetId = `issue:${repo}:${issueNumber}`;
  const loop = createLoopRecord({
    context,
    projectId,
    type: "planner",
    targetType: "issue",
    targetId,
    repo,
    issueNumber,
    status: "running",
    now,
    metadataJson: JSON.stringify({ issueNumber }),
  });

  new SchedulerQueue({
    store: context.store,
    retryMaxAttempts: context.config.scheduler.retryMaxAttempts,
    retryBaseDelayMs: context.config.scheduler.retryBaseDelayMs,
    now: () => new Date(now),
  }).enqueue({
    projectId,
    loopId: loop.id,
    type: "planner",
    targetType: "issue",
    targetId,
    repo,
    dedupeKey: `planner:${repo}:${issueNumber}`,
    lockKey: `issue:${repo}:${issueNumber}`,
    payloadJson: JSON.stringify({ issueNumber }),
  });

  return {
    ...loop,
    issueNumber,
  };
}

function buildRunsResponse(
  context: LooperdApiContext,
  searchParams: URLSearchParams,
) {
  const loopId = searchParams.get("loopId")?.trim();

  return {
    items: loopId
      ? context.store.runs.listByLoop(loopId)
      : context.store.runs.list(),
  };
}

interface ActiveRunsQuery {
  type?: string;
  projectId?: string;
  repo?: string;
  prNumber?: number;
}

interface ActiveRunAgentSummary {
  active: true;
  activeCount: number;
  executionId: string;
  vendor: string;
  pid: number | null;
  startedAt: string;
  lastHeartbeatAt: string | null;
  heartbeatCount: number;
  status: string;
}

interface ActiveRunView {
  seq: number;
  runId: string | null;
  loopId: string;
  projectId: string;
  type: string;
  status: string;
  currentStep: string | null;
  startedAt: string | null;
  target:
    | {
        type: "project";
        projectId: string;
        label: string;
      }
    | {
        type: "issue";
        repo: string;
        issueNumber: number;
        label: string;
      }
    | {
        type: "pull_request";
        repo: string;
        prNumber: number;
        label: string;
      };
  agent: ActiveRunAgentSummary | null;
  worktree: {
    id: string | null;
    path: string;
    branch: string | null;
  } | null;
}

function buildActiveRunsResponse(
  context: LooperdApiContext,
  searchParams: URLSearchParams,
) {
  const query = readActiveRunsQuery(searchParams);
  const items = buildActiveRunViews(context).filter((item) =>
    matchesActiveRunsQuery(item, query),
  );

  return { items };
}

function buildActiveRunViews(context: LooperdApiContext): ActiveRunView[] {
  const activeRuns = context.store.runs.listByStatus("running");
  const queuedLoops = context.store.loops
    .list()
    .filter((loop) => loop.status === "queued");
  const activeAgentByRunId = buildActiveAgentByRunId(
    context.store.agentExecutions.listActive(),
  );

  const runningViews = activeRuns
    .map<ActiveRunView | null>((run) => {
      const loop = context.store.loops.getById(run.loopId);
      if (!loop) {
        return null;
      }

      const target = tryBuildActiveRunTarget(context, loop);
      if (!target) {
        return null;
      }

      return {
        seq: loop.seq,
        runId: run.id,
        loopId: run.loopId,
        projectId: loop.projectId,
        type: loop.type,
        status: run.status,
        currentStep: run.currentStep ?? null,
        startedAt: run.startedAt,
        target,
        agent: activeAgentByRunId.get(run.id) ?? null,
        worktree: buildWorktreeSummary(loop, run),
      } satisfies ActiveRunView;
    })
    .filter(isActiveRunView);

  const queuedViews = queuedLoops
    .map<ActiveRunView | null>((loop) => {
      const target = tryBuildActiveRunTarget(context, loop);
      if (!target) {
        return null;
      }

      return {
        seq: loop.seq,
        runId: null,
        loopId: loop.id,
        projectId: loop.projectId,
        type: loop.type,
        status: loop.status,
        currentStep: null,
        startedAt: loop.nextRunAt ?? loop.updatedAt ?? loop.createdAt,
        target,
        agent: null,
        worktree: null,
      } satisfies ActiveRunView;
    })
    .filter(isActiveRunView);

  return [...runningViews, ...queuedViews].sort(compareActiveRunViews);
}

function buildActiveAgentByRunId(
  executions: AgentExecutionRecord[],
): Map<string, ActiveRunAgentSummary> {
  const grouped = new Map<string, AgentExecutionRecord[]>();

  for (const execution of executions) {
    if (!execution.runId) {
      continue;
    }

    const bucket = grouped.get(execution.runId) ?? [];
    bucket.push(execution);
    grouped.set(execution.runId, bucket);
  }

  return new Map(
    Array.from(grouped.entries()).map(([runId, bucket]) => {
      const sorted = [...bucket].sort((left, right) =>
        compareIsoDesc(left.startedAt, right.startedAt),
      );
      const primary = sorted[0];
      if (!primary) {
        throw new Error(`Missing active execution for run ${runId}`);
      }

      return [
        runId,
        {
          active: true,
          activeCount: bucket.length,
          executionId: primary.id,
          vendor: primary.vendor,
          pid: primary.pid ?? null,
          startedAt: primary.startedAt,
          lastHeartbeatAt: primary.lastHeartbeatAt ?? null,
          heartbeatCount: primary.heartbeatCount,
          status: primary.status,
        } satisfies ActiveRunAgentSummary,
      ];
    }),
  );
}

function isActiveRunView(item: ActiveRunView | null): item is ActiveRunView {
  return item !== null;
}

function tryBuildActiveRunTarget(
  context: LooperdApiContext,
  loop: LoopRecord,
): ActiveRunView["target"] | null {
  if (loop.targetType === "project") {
    const projectId =
      loop.targetId?.startsWith("project:") === true
        ? loop.targetId.slice("project:".length)
        : loop.targetId;
    if (!projectId) {
      return null;
    }

    const project = context.store.projects.getById(projectId);

    return {
      type: "project",
      projectId,
      label: project?.name?.trim() || projectId,
    };
  }

  if (loop.targetType === "issue") {
    const issueNumber = parseIssueNumber(loop.targetId);
    if (!loop.repo || !issueNumber) {
      return null;
    }

    return {
      type: "issue",
      repo: loop.repo,
      issueNumber,
      label: `${loop.repo}#${issueNumber}`,
    };
  }

  if (!loop.repo || !loop.prNumber) {
    return null;
  }

  return {
    type: "pull_request",
    repo: loop.repo,
    prNumber: loop.prNumber,
    label: `${loop.repo}#${loop.prNumber}`,
  };
}

function readActiveRunsQuery(searchParams: URLSearchParams): ActiveRunsQuery {
  const type = searchParams.get("type")?.trim() || undefined;
  const projectId = searchParams.get("projectId")?.trim() || undefined;
  const repo = searchParams.get("repo")?.trim() || undefined;
  const prNumberValue = searchParams.get("prNumber")?.trim();
  const prNumber = prNumberValue
    ? readSearchParamPositiveInteger(prNumberValue, "prNumber")
    : undefined;

  if ((repo && prNumber === undefined) || (!repo && prNumber !== undefined)) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "repo and prNumber must be provided together",
    );
  }

  return { type, projectId, repo, prNumber };
}

function readSearchParamPositiveInteger(
  value: string,
  fieldName: string,
): number {
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      `${fieldName} must be a positive integer`,
    );
  }

  return parsed;
}

function matchesActiveRunsQuery(
  item: ActiveRunView,
  query: ActiveRunsQuery,
): boolean {
  if (query.type && item.type !== query.type) {
    return false;
  }

  if (query.projectId && item.projectId !== query.projectId) {
    return false;
  }

  if (query.repo || query.prNumber !== undefined) {
    if (
      item.target.type !== "pull_request" ||
      item.target.repo !== query.repo ||
      item.target.prNumber !== query.prNumber
    ) {
      return false;
    }
  }

  return true;
}

function compareActiveRunViews(
  left: ActiveRunView,
  right: ActiveRunView,
): number {
  const leftIsRunning = left.status === "running" ? 1 : 0;
  const rightIsRunning = right.status === "running" ? 1 : 0;
  if (leftIsRunning !== rightIsRunning) {
    return rightIsRunning - leftIsRunning;
  }

  const leftHasActiveAgent = left.agent ? 1 : 0;
  const rightHasActiveAgent = right.agent ? 1 : 0;
  if (leftHasActiveAgent !== rightHasActiveAgent) {
    return rightHasActiveAgent - leftHasActiveAgent;
  }

  const startedAtComparison = compareIsoAsc(
    left.startedAt ?? "",
    right.startedAt ?? "",
  );
  if (startedAtComparison !== 0) {
    return startedAtComparison;
  }

  return (left.runId ?? left.loopId).localeCompare(right.runId ?? right.loopId);
}

function compareIsoAsc(left: string, right: string): number {
  return left.localeCompare(right);
}

function compareIsoDesc(left: string, right: string): number {
  return right.localeCompare(left);
}

async function buildProjectsRouteResponse(
  context: LooperdApiContext,
  request: Request,
) {
  if (request.method === "GET") {
    return {
      items: context.store.projects
        .list()
        .map((project) => serializeProject(project)),
    };
  }

  assertMethod(request, ["POST"], "/api/v1/projects");
  if (!context.projects) {
    throw new ApiError(
      "PROJECTS_UNAVAILABLE",
      500,
      "Project management is not available in this runtime",
    );
  }

  const body = await parseJsonBody(request);
  const repoPath = readRequiredString(body, "repoPath");
  const providedId = readOptionalString(body, "id");
  const id = providedId ?? deriveProjectIdFromRepoPath(repoPath);
  const name = readOptionalString(body, "name") ?? id;
  const baseBranch =
    readOptionalString(body, "baseBranch") ??
    context.config.defaults.baseBranch;

  const result = await context.projects.addProject({
    id,
    name,
    repoPath,
    baseBranch,
    idSource: providedId ? "explicit" : "derived",
    worktreeRoot: readOptionalString(body, "worktreeRoot"),
    repo: readOptionalString(body, "repo"),
  });

  return {
    ...serializeProject(result.project),
    repo: result.repo,
    discoveredPullRequests: result.discoveredPullRequests,
    discoveredWorktrees: result.discoveredWorktrees,
    warnings: result.warnings,
  };
}

async function buildLoopsCreateResponse(
  context: LooperdApiContext,
  request: Request,
) {
  assertMethod(request, ["POST"], "/api/v1/loops");
  const body = await parseJsonBody(request);
  const projectId = readRequiredString(body, "projectId");

  if (!context.store.projects.getById(projectId)) {
    throw new ApiError(
      "PROJECT_NOT_FOUND",
      404,
      `Project not found: ${projectId}`,
    );
  }

  const type = readRequiredString(body, "type");
  const targetType = readRequiredString(body, "targetType");
  const status = readOptionalString(body, "status") ?? "running";
  const metadata = readOptionalObject(body, "metadata");
  const now = new Date().toISOString();

  if (
    (type === "reviewer" || type === "fixer") &&
    !isCodingAgentConfigured(context.config)
  ) {
    throw new ApiError(
      "AGENT_NOT_CONFIGURED",
      400,
      `Cannot create ${type} loop without config.agent.vendor`,
    );
  }

  const loop = createLoopRecord({
    context,
    projectId,
    type,
    targetType,
    targetId: readOptionalString(body, "targetId") ?? undefined,
    repo: readOptionalString(body, "repo") ?? undefined,
    prNumber: readOptionalPositiveInteger(body, "prNumber") ?? undefined,
    issueNumber: readOptionalPositiveInteger(body, "issueNumber") ?? undefined,
    status,
    now,
    metadataJson: metadata ? JSON.stringify(metadata) : null,
  });

  enqueueLoopCreate(context, loop, now);

  return loop;
}

function enqueueLoopCreate(
  context: LooperdApiContext,
  loop: LoopRecord,
  now: string,
): void {
  const scheduler = new SchedulerQueue({
    store: context.store,
    retryMaxAttempts: context.config.scheduler.retryMaxAttempts,
    retryBaseDelayMs: context.config.scheduler.retryBaseDelayMs,
    now: () => new Date(now),
  });

  if (
    loop.type === "reviewer" &&
    loop.status === "running" &&
    loop.repo &&
    loop.prNumber
  ) {
    scheduler.enqueue({
      projectId: loop.projectId,
      loopId: loop.id,
      type: "reviewer",
      targetType: "pull_request",
      targetId: `pr:${loop.repo}:${loop.prNumber}`,
      repo: loop.repo,
      prNumber: loop.prNumber,
      dedupeKey: `reviewer:${loop.repo}:${loop.prNumber}`,
      lockKey: createPrLockKey(loop.repo, loop.prNumber),
    });
  }
}

function createLoopRecord(input: {
  context: LooperdApiContext;
  projectId: string;
  type: string;
  targetType: string;
  targetId?: string;
  repo?: string;
  prNumber?: number;
  issueNumber?: number;
  status: string;
  now: string;
  metadataJson?: string | null;
}) {
  const { context } = input;
  return context.store.withTransaction(() => {
    const issueNumber =
      input.issueNumber ??
      (input.targetType === "issue"
        ? parseIssueNumber(input.targetId)
        : undefined);
    const target =
      input.targetType === "project"
        ? defineProjectLoopTarget(readRequiredValue(input.targetId, "targetId"))
        : input.targetType === "issue"
          ? defineIssueLoopTarget(
              readRequiredValue(input.repo, "repo"),
              readRequiredNumber(issueNumber, "issueNumber"),
            )
          : definePullRequestLoopTarget(
              readRequiredValue(input.repo, "repo"),
              readRequiredNumber(input.prNumber, "prNumber"),
            );

    const loop = createLoop({
      id: randomUUID(),
      projectId: input.projectId,
      type: input.type as never,
      target,
      status: input.status as never,
      createdAt: input.now,
      updatedAt: input.now,
    });

    const existingLoops = context.store.loops.list().map((candidate) => ({
      id: candidate.id,
      projectId: candidate.projectId,
      type: candidate.type as never,
      status: candidate.status as never,
      target: toLoopTarget(candidate),
    }));

    try {
      assertUniqueActiveLoop({
        loops: existingLoops,
        candidate: loop,
      });
    } catch (error) {
      throw new ApiError(
        "LOOP_CONFLICT",
        409,
        error instanceof Error ? error.message : "Active loop already exists",
      );
    }

    const record = {
      id: loop.id,
      seq: context.store.loops.allocateSeq(),
      projectId: loop.projectId,
      type: loop.type,
      targetType: target.targetType,
      targetId:
        target.targetType === "project"
          ? `project:${target.projectId}`
          : target.targetType === "issue"
            ? `issue:${target.repo}:${target.issueNumber}`
            : `pr:${target.repo}:${target.prNumber}`,
      repo:
        target.targetType === "pull_request" || target.targetType === "issue"
          ? target.repo
          : null,
      prNumber: target.targetType === "pull_request" ? target.prNumber : null,
      status: loop.status,
      configJson: null,
      metadataJson: input.metadataJson ?? null,
      lastRunAt: null,
      nextRunAt: loop.status === "running" ? input.now : null,
      createdAt: input.now,
      updatedAt: input.now,
    };

    context.store.loops.upsert(record);
    return record;
  });
}

function resolveLoop(context: LooperdApiContext, selector: string): LoopRecord {
  const normalized = selector.trim();
  if (/^\d+$/.test(normalized)) {
    const loopBySeq = context.store.loops.getBySeq(Number(normalized));
    if (loopBySeq) {
      return loopBySeq;
    }
  }

  const loopById = context.store.loops.getById(normalized);
  if (!loopById) {
    throw new ApiError("LOOP_NOT_FOUND", 404, `Loop not found: ${selector}`);
  }

  return loopById;
}

function buildLoopLogsResponse(context: LooperdApiContext, loop: LoopRecord) {
  const latestRun = context.store.runs.getLatestByLoopId(loop.id);
  const latestAgent = latestRun
    ? context.store.agentExecutions.getLatestByRunId(latestRun.id)
    : null;
  const output = parseOutputJson(latestAgent?.outputJson);

  return {
    seq: loop.seq,
    loopId: loop.id,
    loopType: loop.type,
    loopStatus: loop.status,
    run: latestRun
      ? {
          runId: latestRun.id,
          status: latestRun.status,
          currentStep: latestRun.currentStep ?? null,
          startedAt: latestRun.startedAt,
          endedAt: latestRun.endedAt ?? null,
          summary: latestRun.summary ?? null,
          errorMessage: latestRun.errorMessage ?? null,
        }
      : null,
    agent: latestAgent
      ? {
          executionId: latestAgent.id,
          vendor: latestAgent.vendor,
          status: latestAgent.status,
          pid: latestAgent.pid ?? null,
          startedAt: latestAgent.startedAt,
          endedAt: latestAgent.endedAt ?? null,
          heartbeatCount: latestAgent.heartbeatCount,
          lastHeartbeatAt: latestAgent.lastHeartbeatAt ?? null,
          summary: latestAgent.summary ?? null,
          parseStatus: latestAgent.parseStatus ?? null,
          stdout: output.stdout,
          stderr: output.stderr,
        }
      : null,
  };
}

function buildWorktreeSummary(
  loop: LoopRecord,
  run: RunRecord,
): ActiveRunView["worktree"] {
  const checkpoint = asObject(parsePayloadJson(run.checkpointJson ?? "null"));
  const checkpointWorktree = asObject(checkpoint.worktree);
  const loopMetadata = asObject(parsePayloadJson(loop.metadataJson ?? "null"));
  const path =
    readString(checkpointWorktree.path) ??
    readString(loopMetadata.worktreePath);

  if (!path) {
    return null;
  }

  return {
    id:
      readString(checkpointWorktree.id) ?? readString(loopMetadata.worktreeId),
    path,
    branch:
      readString(checkpointWorktree.branch) ?? readString(loopMetadata.branch),
  };
}

function toLoopTarget(loop: ReturnType<Store["loops"]["list"]>[number]) {
  if (loop.targetType === "project") {
    const projectId =
      loop.targetId?.startsWith("project:") === true
        ? loop.targetId.slice("project:".length)
        : loop.targetId;

    return defineProjectLoopTarget(
      readRequiredValue(projectId ?? undefined, "projectId"),
    );
  }

  if (loop.targetType === "issue") {
    return defineIssueLoopTarget(
      readRequiredValue(loop.repo ?? undefined, "repo"),
      readRequiredNumber(
        parseIssueNumber(loop.targetId) ?? undefined,
        "issueNumber",
      ),
    );
  }

  return definePullRequestLoopTarget(
    readRequiredValue(loop.repo ?? undefined, "repo"),
    readRequiredNumber(loop.prNumber ?? undefined, "prNumber"),
  );
}

function parseIssueNumber(
  targetId: string | null | undefined,
): number | undefined {
  const match = /^issue:[^:]+\/[^:]+:(\d+)$/.exec(targetId ?? "");
  return match ? Number(match[1]) : undefined;
}

function readRequiredValue(
  value: string | undefined,
  fieldName: string,
): string {
  if (!value) {
    throw new ApiError("VALIDATION_FAILED", 400, `${fieldName} is required`);
  }

  return value;
}

function readRequiredNumber(
  value: number | undefined,
  fieldName: string,
): number {
  if (!value) {
    throw new ApiError("VALIDATION_FAILED", 400, `${fieldName} is required`);
  }

  return value;
}

async function parseJsonBody(
  request: Request,
): Promise<Record<string, unknown>> {
  try {
    const body = await request.json();
    if (!body || typeof body !== "object" || Array.isArray(body)) {
      throw new ApiError(
        "VALIDATION_FAILED",
        400,
        "request body must be a JSON object",
      );
    }

    return body as Record<string, unknown>;
  } catch (error) {
    if (error instanceof ApiError) {
      throw error;
    }

    throw new ApiError("VALIDATION_FAILED", 400, "invalid JSON request body");
  }
}

function readRequiredString(
  body: Record<string, unknown>,
  fieldName: string,
): string {
  const value = body[fieldName];
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      `${fieldName} must be a non-empty string`,
    );
  }

  return value.trim();
}

function readOptionalString(
  body: Record<string, unknown>,
  fieldName: string,
): string | null {
  const value = body[fieldName];
  if (value == null) {
    return null;
  }

  if (typeof value !== "string") {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      `${fieldName} must be a string`,
    );
  }

  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

function readOptionalObject(
  body: Record<string, unknown>,
  fieldName: string,
): Record<string, unknown> | null {
  const value = body[fieldName];
  if (value == null) {
    return null;
  }

  if (typeof value !== "object" || Array.isArray(value)) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      `${fieldName} must be an object`,
    );
  }

  return value as Record<string, unknown>;
}

function readOptionalPositiveInteger(
  body: Record<string, unknown>,
  fieldName: string,
): number | null {
  const value = body[fieldName];
  if (value == null) {
    return null;
  }

  if (typeof value !== "number" || !Number.isInteger(value) || value <= 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      `${fieldName} must be a positive integer`,
    );
  }

  return value as number;
}

function buildPullRequestRouteResponse(
  context: LooperdApiContext,
  pathname: string,
) {
  const parts = pathname.split("/").filter(Boolean);
  const encodedRepo = parts[3];
  const maybePrNumber = parts[4];
  const subresource = parts[5];

  if (!encodedRepo || !maybePrNumber) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "repo and prNumber are required",
    );
  }

  const repo = decodeURIComponent(encodedRepo);
  const prNumber = Number(maybePrNumber);

  if (!Number.isInteger(prNumber) || prNumber <= 0) {
    throw new ApiError(
      "VALIDATION_FAILED",
      400,
      "prNumber must be a positive integer",
    );
  }

  const snapshot = context.store.pullRequestSnapshots.getLatest(repo, prNumber);

  if (!snapshot) {
    throw new ApiError(
      "PR_NOT_FOUND",
      404,
      `Pull request not found: ${repo}#${prNumber}`,
    );
  }

  if (subresource === "status") {
    return buildPullRequestStatusResponse(context, snapshot);
  }

  if (subresource) {
    throw new ApiError("ROUTE_NOT_FOUND", 404, `Unknown route: ${pathname}`);
  }

  return serializePullRequest(context, snapshot);
}

function buildPullRequestStatusResponse(
  context: LooperdApiContext,
  snapshot: PullRequestSnapshotRecord,
) {
  const loopMatches = findPullRequestLoops(
    context,
    snapshot.repo,
    snapshot.prNumber,
  );
  const runMatches = loopMatches.flatMap((loop) =>
    context.store.runs.listByLoop(loop.id),
  );
  return {
    repo: snapshot.repo,
    prNumber: snapshot.prNumber,
    reviewState: snapshot.reviewState,
    checksSummary: snapshot.checksSummary,
    unresolvedThreadCount: snapshot.unresolvedThreadCount ?? 0,
    capturedAt: snapshot.capturedAt,
    reviewer: findLatestLoopStatus(loopMatches, "reviewer"),
    fixer: findLatestLoopStatus(loopMatches, "fixer"),
    loopStatus: summarizeRunAndLoopState(loopMatches, runMatches),
  };
}

function findPullRequestLoops(
  context: LooperdApiContext,
  repo: string,
  prNumber: number,
) {
  return context.store.loops
    .list()
    .filter((loop) => loop.repo === repo && loop.prNumber === prNumber);
}

function findLatestLoopStatus(
  loops: { type: string; status: string }[],
  type: "reviewer" | "fixer",
) {
  return loops.find((loop) => loop.type === type)?.status ?? null;
}

function collectPullRequestIdentities(
  snapshots: PullRequestSnapshotRecord[],
  loops: ReturnType<Store["loops"]["list"]>,
) {
  const seen = new Set<string>();
  const identities: Array<{
    repo: string;
    prNumber: number;
    projectId: string;
  }> = [];

  const appendIdentity = (item: {
    repo?: string | null;
    prNumber?: number | null;
    projectId: string;
  }) => {
    if (!item.repo || !item.prNumber) {
      return;
    }

    const key = `${item.repo}#${item.prNumber}`;
    if (seen.has(key)) {
      return;
    }

    seen.add(key);
    identities.push({
      repo: item.repo,
      prNumber: item.prNumber,
      projectId: item.projectId,
    });
  };

  for (const snapshot of snapshots) {
    appendIdentity(snapshot);
  }

  for (const loop of loops) {
    appendIdentity(loop);
  }
  return identities;
}

function summarizeRunAndLoopState(
  loopMatches: { status: string }[],
  runs: RunRecord[],
) {
  return {
    loops: loopMatches.map((loop) => loop.status),
    latestRunStatus: runs[0]?.status,
    runningRunCount: runs.filter((run) => run.status === "running").length,
  };
}

function dedupeLatestSnapshots(
  snapshots: PullRequestSnapshotRecord[],
): PullRequestSnapshotRecord[] {
  const seen = new Set<string>();
  const deduped: PullRequestSnapshotRecord[] = [];

  for (const snapshot of snapshots) {
    const key = `${snapshot.repo}#${snapshot.prNumber}`;
    if (seen.has(key)) {
      continue;
    }

    seen.add(key);
    deduped.push(snapshot);
  }

  return deduped;
}

function serializePullRequest(
  context: LooperdApiContext,
  snapshot: PullRequestSnapshotRecord,
) {
  return serializePullRequestListItem(
    context,
    snapshot.repo,
    snapshot.prNumber,
    snapshot,
  );
}

function serializePullRequestListItem(
  context: LooperdApiContext,
  repo: string,
  prNumber: number,
  snapshot: PullRequestSnapshotRecord | undefined,
) {
  const loopMatches = findPullRequestLoops(context, repo, prNumber);

  return {
    repo,
    prNumber,
    projectId: snapshot?.projectId ?? loopMatches[0]?.projectId ?? null,
    headSha: snapshot?.headSha ?? null,
    baseSha: snapshot?.baseSha ?? null,
    title: snapshot?.title ?? null,
    body: snapshot?.body ?? null,
    author: snapshot?.author ?? null,
    diffRef: snapshot?.diffRef ?? null,
    checksSummary: snapshot?.checksSummary ?? null,
    unresolvedThreadCount: snapshot?.unresolvedThreadCount ?? 0,
    reviewState: snapshot?.reviewState ?? null,
    capturedAt: snapshot?.capturedAt ?? null,
    reviewer: findLatestLoopStatus(loopMatches, "reviewer"),
    fixer: findLatestLoopStatus(loopMatches, "fixer"),
  };
}

function serializeProject(
  project: ReturnType<Store["projects"]["list"]>[number],
) {
  const metadata = parseMetadata(project.metadataJson);

  return {
    id: project.id,
    name: project.name,
    repoPath: project.repoPath,
    baseBranch: project.baseBranch,
    archived: project.archived,
    repo:
      typeof metadata?.repo === "string" && metadata.repo.length > 0
        ? metadata.repo
        : null,
    worktreeRoot:
      typeof metadata?.worktreeRoot === "string" &&
      metadata.worktreeRoot.length > 0
        ? metadata.worktreeRoot
        : null,
    createdAt: project.createdAt,
    updatedAt: project.updatedAt,
  };
}

function parseMetadata(metadataJson?: string | null): Record<string, unknown> {
  const parsed = parsePayloadJson(metadataJson ?? "null");
  return parsed && typeof parsed === "object" && !Array.isArray(parsed)
    ? (parsed as Record<string, unknown>)
    : {};
}

function resolveWorkerProject(
  context: LooperdApiContext,
  input: {
    projectId: string | null;
    repo: string | null;
    prNumber: number | null;
  },
) {
  if (input.projectId) {
    const project = context.store.projects.getById(input.projectId);
    if (!project) {
      throw new ApiError(
        "PROJECT_NOT_FOUND",
        404,
        `Project not found: ${input.projectId}`,
      );
    }
    return project;
  }

  if (input.repo && input.prNumber) {
    const snapshots = context.store.pullRequestSnapshots
      .list()
      .filter(
        (snapshot) =>
          snapshot.repo === input.repo && snapshot.prNumber === input.prNumber,
      );
    const projectIds = [
      ...new Set(snapshots.map((snapshot) => snapshot.projectId)),
    ];
    if (projectIds.length > 1) {
      throw new ApiError(
        "PROJECT_AMBIGUOUS",
        409,
        `Multiple projects match pull request ${input.repo}#${input.prNumber}; pass projectId explicitly`,
      );
    }
    const projectId = projectIds[0];
    if (projectId) {
      const project = context.store.projects.getById(projectId);
      if (project) {
        return project;
      }
    }
  }

  if (input.repo) {
    const matches = context.store.projects.list().filter((project) => {
      const metadata = parseMetadata(project.metadataJson);
      return readString(metadata.repo) === input.repo;
    });
    if (matches.length === 1) {
      const project = matches[0];
      if (project) {
        return project;
      }
    }
    if (matches.length > 1) {
      throw new ApiError(
        "PROJECT_AMBIGUOUS",
        409,
        `Multiple projects match repo ${input.repo}; pass projectId explicitly`,
      );
    }
  }

  throw new ApiError(
    "VALIDATION_FAILED",
    400,
    "projectId is required unless it can be resolved from repo/prNumber",
  );
}

function requirePullRequestTarget(
  context: LooperdApiContext,
  input: {
    projectId: string;
    repo: string;
    prNumber: number;
  },
): { prNumber: number } {
  const snapshot = context.store.pullRequestSnapshots.getLatest(
    input.repo,
    input.prNumber,
  );
  if (!snapshot) {
    throw new ApiError(
      "PULL_REQUEST_NOT_FOUND",
      404,
      `Pull request not found: ${input.repo}#${input.prNumber}`,
    );
  }
  const project = context.store.projects.getById(input.projectId);
  if (!project) {
    throw new ApiError(
      "PROJECT_NOT_FOUND",
      404,
      `Project not found: ${input.projectId}`,
    );
  }

  const projectMetadata = parseMetadata(project.metadataJson);
  if (readString(projectMetadata.repo) !== input.repo) {
    throw new ApiError(
      "PULL_REQUEST_PROJECT_MISMATCH",
      409,
      `Pull request ${input.repo}#${input.prNumber} does not belong to project ${input.projectId}`,
    );
  }

  return { prNumber: snapshot.prNumber };
}

function maybeFindPlannerLoopForIssue(
  context: LooperdApiContext,
  input: {
    projectId: string;
    repo: string;
    issueNumber: number;
  },
): { prNumber: number | null; specPath: string | null } | null {
  const targetId = `issue:${input.repo}:${input.issueNumber}`;
  const loop = context.store.loops
    .list()
    .find(
      (candidate) =>
        candidate.projectId === input.projectId &&
        candidate.type === "planner" &&
        candidate.targetType === "issue" &&
        candidate.targetId === targetId,
    );
  if (!loop) {
    return null;
  }

  const metadata = parseMetadata(loop.metadataJson);
  const prNumber = loop.prNumber ?? readNumber(metadata.prNumber) ?? null;

  return {
    prNumber,
    specPath: readString(metadata.specPath),
  };
}

function readString(value: unknown): string | null {
  return typeof value === "string" && value.trim().length > 0
    ? value.trim()
    : null;
}

function asObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function parseOutputJson(outputJson?: string | null): {
  stdout: string;
  stderr: string;
} {
  const parsed = asObject(parsePayloadJson(outputJson ?? "null"));
  return {
    stdout: readString(parsed.stdout) ?? "",
    stderr: readString(parsed.stderr) ?? "",
  };
}

function readNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isInteger(value) && value > 0
    ? value
    : null;
}

function deriveWorkerTitle(input: {
  prompt: string | null;
  specPath: string | null;
  repo: string | null;
  prNumber: number | null;
  issueNumber: number | null;
}): string {
  if (input.prompt) {
    return input.prompt.slice(0, 80);
  }

  if (input.specPath) {
    return `Implement ${input.specPath}`;
  }

  if (input.prNumber && input.repo) {
    return `Implement ${input.repo}#${input.prNumber}`;
  }

  if (input.issueNumber && input.repo) {
    return `Implement ${input.repo}#${input.issueNumber}`;
  }

  return "Worker run";
}

function buildWorkerPullRequestDedupeKey(
  projectId: string,
  repo: string,
  prNumber: number,
): string {
  return `worker:${projectId}:${repo}:${prNumber}`;
}

function buildWorkerPullRequestLockKey(repo: string, prNumber: number): string {
  return createPrLockKey(repo, prNumber);
}

function serializeEvent(event: EventLogRecord) {
  return {
    ...event,
    payload: parsePayloadJson(event.payloadJson),
  };
}

function parsePayloadJson(payloadJson: string): unknown {
  try {
    return JSON.parse(payloadJson);
  } catch {
    return payloadJson;
  }
}

function jsonResponse<T>(status: number, payload: ApiResponse<T>): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
    },
  });
}

function isCodingAgentConfigured(config: LooperConfig): boolean {
  return Boolean(config.agent.vendor);
}

function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) {
    return error;
  }

  if (error instanceof InvalidProjectIdError) {
    return new ApiError("VALIDATION_FAILED", 400, error.message);
  }

  if (error instanceof ProjectIdCollisionError) {
    return new ApiError("PROJECT_ID_CONFLICT", 409, error.message);
  }

  return new ApiError(
    "INTERNAL_ERROR",
    500,
    error instanceof Error ? error.message : "Unknown error",
  );
}
