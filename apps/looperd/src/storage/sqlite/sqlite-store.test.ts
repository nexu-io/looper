import { afterEach, describe, expect, test } from "bun:test";
import { access, mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { Database } from "bun:sqlite";

import { SqliteStore } from "./sqlite-store";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function createStoreFixture() {
  const rootDir = await mkdtemp(join(tmpdir(), "looper-store-"));
  cleanupPaths.push(rootDir);

  return {
    rootDir,
    dbPath: join(rootDir, "state", "looper.sqlite"),
    backupDir: join(rootDir, "backups"),
  };
}

describe("SqliteStore", () => {
  test("resolves loops by seq and allocates past seeded sequence values", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({
      dbPath: fixture.dbPath,
      backupDir: fixture.backupDir,
    });

    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_3",
      seq: 3,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_7",
      seq: 7,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });

    expect(store.loops.getBySeq(7)?.id).toBe("loop_7");

    const db = new Database(fixture.dbPath);
    db.query("DELETE FROM counters WHERE name = 'loop_seq'").run();
    db.close();

    expect(store.loops.allocateSeq()).toBe(8);
    expect(store.loops.allocateSeq()).toBe(9);

    store.close();
  });

  test("initializes schema, writes records, and reports health", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({
      dbPath: fixture.dbPath,
      backupDir: fixture.backupDir,
    });

    store.initialize({ autoMigrate: true, requireBackup: true });

    const now = "2026-04-11T12:00:00.000Z";

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: '{"tier":"mvp"}',
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_1",
      seq: 1,
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "idle",
      configJson: '{"priority":"normal"}',
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.runs.upsert({
      id: "run_1",
      loopId: "loop_1",
      status: "running",
      currentStep: "snapshot",
      lastCompletedStep: null,
      checkpointJson: '{"cursor":1}',
      summary: null,
      errorMessage: null,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
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
      title: "Persistence",
      body: "This adds storage",
      author: "octocat",
      diffRef: "refs/pull/42/head",
      checksSummary: "all-green",
      unresolvedThreadCount: 2,
      reviewState: "changes_requested",
      payloadJson: '{"title":"Persistence"}',
      capturedAt: now,
      createdAt: now,
    });
    store.events.append({
      id: "event_1",
      eventType: "loop.created",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      entityType: "loop",
      entityId: "loop_1",
      correlationId: "corr_1",
      causationId: "cause_1",
      actorType: "agent",
      actorId: "reviewer_1",
      actorDisplayName: "Reviewer",
      payloadJson: '{"status":"idle"}',
      createdAt: now,
    });
    store.queue.upsert({
      id: "queue_reviewer_1",
      projectId: "project_1",
      loopId: "loop_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
      priority: 1,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: '{"source":"discover"}',
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });
    store.queue.upsert({
      id: "queue_fixer_1",
      projectId: "project_1",
      loopId: null,
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "fixer:acme/looper:42",
      priority: 2,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(
      store.locks.acquire({
        key: "pr:acme/looper:42",
        owner: "reviewer-loop",
        reason: "claim",
        expiresAt: "2026-04-11T12:05:00.000Z",
        createdAt: now,
        updatedAt: now,
      }),
    ).toBe(true);
    store.agentExecutions.upsert({
      id: "agent_exec_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      vendor: "opencode",
      status: "running",
      pid: 12345,
      commandJson: '{"command":"opencode","args":["run"]}',
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 3,
      lastHeartbeatAt: now,
      outputJson: '{"stdout":"ok","stderr":""}',
      errorMessage: null,
      startedAt: now,
      endedAt: null,
      metadataJson: '{"idempotencyKey":"abc"}',
      createdAt: now,
      updatedAt: now,
    });
    store.notifications.upsert({
      id: "notification_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      entityType: "loop",
      entityId: "loop_1",
      channel: "in_app",
      level: "info",
      title: "Loop updated",
      subtitle: "loop_1",
      body: "Worker progressed",
      status: "success",
      dedupeKey: "loop.updated:loop:loop_1",
      errorMessage: null,
      payloadJson: '{"title":"Loop updated"}',
      sentAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.worktrees.upsert({
      id: "worktree_1",
      projectId: "project_1",
      repoPath: "/tmp/looper",
      worktreePath: "/tmp/looper-worktrees/feature-loop-1",
      branch: "feature/loop-1",
      baseBranch: "main",
      status: "active",
      headSha: "abc123",
      metadataJson: '{"recovered":false}',
      createdAt: now,
      updatedAt: now,
      cleanedAt: null,
    });

    expect(store.projects.getById("project_1")).toEqual({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: '{"tier":"mvp"}',
      createdAt: now,
      updatedAt: now,
    });
    expect(store.loops.getById("loop_1")?.repo).toBe("acme/looper");
    expect(store.loops.getById("loop_1")?.projectId).toBe("project_1");
    expect(store.runs.listByLoop("loop_1")).toHaveLength(1);
    expect(store.runs.getLatestByLoopId("loop_1")?.id).toBe("run_1");
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.headSha,
    ).toBe("abc123");
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.projectId,
    ).toBe("project_1");
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.baseSha,
    ).toBe("base123");
    expect(store.events.listByEntity("loop", "loop_1")).toHaveLength(1);
    expect(store.events.listByEntity("loop", "loop_1")[0]?.actorId).toBe(
      "reviewer_1",
    );
    expect(store.locks.get("pr:acme/looper:42")?.owner).toBe("reviewer-loop");
    expect(store.queue.findActiveByDedupe("reviewer:acme/looper:42")?.id).toBe(
      "queue_reviewer_1",
    );
    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_reviewer_1",
    ]);
    expect(store.queue.claimNext(now, "executor_1")?.id).toBe(
      "queue_reviewer_1",
    );
    expect(store.queue.getById("queue_reviewer_1")?.status).toBe("running");
    store.queue.complete("queue_reviewer_1", now);
    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_fixer_1",
    ]);
    expect(store.agentExecutions.listActive()).toHaveLength(1);
    expect(store.agentExecutions.getById("agent_exec_1")?.pid).toBe(12345);
    expect(store.notifications.list(1)[0]?.channel).toBe("in_app");
    expect(
      store.notifications.getLatestByDedupe(
        "in_app",
        "loop.updated:loop:loop_1",
      )?.status,
    ).toBe("success");
    expect(
      store.worktrees.getByBranch("project_1", "feature/loop-1")?.status,
    ).toBe("active");

    const health = store.schema.healthcheck();
    expect(health.ok).toBe(true);
    expect(health.migration.latestAppliedId).toBe(
      "0007_agent_execution_run_index",
    );
    expect(health.lastUpdatedAt).toBeString();

    const backupPath = store.schema.backup();
    await access(backupPath);

    store.close();
  });

  test("migrates legacy task-target worker schemas to project-target worker schemas", async () => {
    const fixture = await createStoreFixture();
    await mkdir(join(fixture.rootDir, "state"), { recursive: true });
    const db = new Database(fixture.dbPath, { create: true });
    db.exec(`
      CREATE TABLE schema_migrations (id TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
      INSERT INTO schema_migrations (id, applied_at) VALUES
        ('0001_init', '2026-04-11T00:00:00.000Z'),
        ('0002_integrations', '2026-04-11T00:00:00.000Z'),
        ('0003_scheduler_queue', '2026-04-11T00:00:00.000Z');

      CREATE TABLE projects (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        repo_path TEXT NOT NULL,
        base_branch TEXT,
        archived INTEGER NOT NULL DEFAULT 0,
        metadata_json TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE loops (
        id TEXT PRIMARY KEY,
        project_id TEXT NOT NULL,
        type TEXT NOT NULL,
        target_type TEXT NOT NULL,
        target_id TEXT,
        repo TEXT,
        pr_number INTEGER,
        status TEXT NOT NULL,
        config_json TEXT,
        metadata_json TEXT,
        last_run_at TEXT,
        next_run_at TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        CHECK (target_type IN ('task', 'pull_request', 'repository', 'manual'))
      );

      CREATE TABLE runs (
        id TEXT PRIMARY KEY,
        loop_id TEXT NOT NULL,
        status TEXT NOT NULL,
        current_step TEXT,
        last_completed_step TEXT,
        checkpoint_json TEXT,
        summary TEXT,
        error_message TEXT,
        started_at TEXT NOT NULL,
        last_heartbeat_at TEXT,
        ended_at TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE queue_items (
        id TEXT PRIMARY KEY,
        project_id TEXT,
        loop_id TEXT,
        task_id TEXT,
        type TEXT NOT NULL,
        target_type TEXT NOT NULL,
        target_id TEXT NOT NULL,
        repo TEXT,
        pr_number INTEGER,
        dedupe_key TEXT NOT NULL,
        priority INTEGER NOT NULL,
        status TEXT NOT NULL,
        available_at TEXT NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0,
        max_attempts INTEGER NOT NULL DEFAULT 3,
        claimed_by TEXT,
        claimed_at TEXT,
        started_at TEXT,
        finished_at TEXT,
        lock_key TEXT,
        payload_json TEXT,
        last_error TEXT,
        last_error_kind TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE agent_executions (
        id TEXT PRIMARY KEY,
        project_id TEXT,
        loop_id TEXT,
        run_id TEXT,
        task_id TEXT,
        vendor TEXT NOT NULL,
        status TEXT NOT NULL,
        pid INTEGER,
        command_json TEXT,
        cwd TEXT,
        summary TEXT,
        parse_status TEXT,
        completion_signal TEXT,
        heartbeat_count INTEGER NOT NULL DEFAULT 0,
        last_heartbeat_at TEXT,
        output_json TEXT,
        error_message TEXT,
        started_at TEXT NOT NULL,
        ended_at TEXT,
        metadata_json TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE worktrees (
        id TEXT PRIMARY KEY,
        project_id TEXT NOT NULL,
        task_id TEXT,
        repo_path TEXT NOT NULL,
        worktree_path TEXT NOT NULL,
        branch TEXT NOT NULL,
        base_branch TEXT,
        status TEXT NOT NULL,
        head_sha TEXT,
        metadata_json TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        cleaned_at TEXT
      );

      CREATE TABLE tasks (
        id TEXT PRIMARY KEY,
        project_id TEXT NOT NULL,
        title TEXT NOT NULL,
        status TEXT NOT NULL,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE task_items (
        id TEXT PRIMARY KEY,
        task_id TEXT NOT NULL,
        content TEXT NOT NULL,
        status TEXT NOT NULL,
        position INTEGER NOT NULL DEFAULT 0,
        source TEXT NOT NULL,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      INSERT INTO projects (id, name, repo_path, base_branch, archived, metadata_json, created_at, updated_at)
      VALUES ('project_1', 'Looper', '/tmp/looper', 'main', 0, NULL, '2026-04-11T00:00:00.000Z', '2026-04-11T00:00:00.000Z');

      INSERT INTO loops (id, project_id, type, target_type, target_id, repo, pr_number, status, config_json, metadata_json, last_run_at, next_run_at, created_at, updated_at)
      VALUES ('loop_worker_1', 'project_1', 'worker', 'task', 'task_1', 'acme/looper', NULL, 'queued', NULL, NULL, NULL, '2026-04-11T00:00:00.000Z', '2026-04-11T00:00:00.000Z', '2026-04-11T00:00:00.000Z');

      INSERT INTO queue_items (id, project_id, loop_id, task_id, type, target_type, target_id, repo, pr_number, dedupe_key, priority, status, available_at, attempts, max_attempts, claimed_by, claimed_at, started_at, finished_at, lock_key, payload_json, last_error, last_error_kind, created_at, updated_at)
      VALUES ('queue_worker_1', 'project_1', 'loop_worker_1', 'task_1', 'worker', 'task', 'task:task_1', 'acme/looper', NULL, 'worker:task_1', 1, 'queued', '2026-04-11T00:00:00.000Z', 0, 3, NULL, NULL, NULL, NULL, 'task:task_1', NULL, NULL, NULL, '2026-04-11T00:00:00.000Z', '2026-04-11T00:00:00.000Z');
    `);
    db.close(false);

    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    expect(store.schema.healthcheck().migration.latestAppliedId).toBe(
      "0007_agent_execution_run_index",
    );
    expect(store.loops.getById("loop_worker_1")).toMatchObject({
      targetType: "project",
      targetId: "project_1",
      status: "paused",
    });
    expect(store.queue.getById("queue_worker_1")).toMatchObject({
      targetType: "project",
      targetId: "project_1",
      dedupeKey: "worker:loop_worker_1",
      status: "cancelled",
      lockKey: "worker:loop_worker_1",
    });
    expect(
      store.withTransaction(() => {
        try {
          store.schema.healthcheck();
          return true;
        } catch {
          return false;
        }
      }),
    ).toBe(true);

    const migratedDb = new Database(fixture.dbPath, { readonly: true });
    const indexes = migratedDb
      .query("PRAGMA index_list('agent_executions')")
      .all() as Array<Record<string, unknown>>;
    migratedDb.close(false);
    expect(
      indexes.some((index) => index.name === "idx_agent_executions_run"),
    ).toBe(true);

    store.close();
  });

  test("rolls back transactional writes on failure", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(() =>
      store.withTransaction((tx) => {
        tx.loops.upsert({
          id: "loop_rollback",
          seq: 1,
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
          nextRunAt: null,
          createdAt: now,
          updatedAt: now,
        });
        tx.events.append({
          id: "event_loop_rollback",
          eventType: "loop.created",
          projectId: "project_1",
          entityType: "loop",
          entityId: "loop_rollback",
          payloadJson: "{}",
          createdAt: now,
        });

        throw new Error("abort transaction");
      }),
    ).toThrow("abort transaction");

    expect(store.loops.getById("loop_rollback")).toBeNull();
    expect(store.events.listByEntity("loop", "loop_rollback")).toHaveLength(0);

    store.close();
  });

  test("re-acquires an expired lock using the injected clock", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({
      dbPath: fixture.dbPath,
      now: () => new Date("2026-04-11T12:10:00.000Z"),
    });
    store.initialize({ autoMigrate: true });

    expect(
      store.locks.acquire({
        key: "task:123",
        owner: "worker-a",
        reason: "initial",
        expiresAt: "2026-04-11T12:00:00.000Z",
        createdAt: "2026-04-11T11:50:00.000Z",
        updatedAt: "2026-04-11T11:50:00.000Z",
      }),
    ).toBe(true);

    expect(
      store.locks.acquire({
        key: "task:123",
        owner: "worker-b",
        reason: "takeover",
        expiresAt: "2026-04-11T12:20:00.000Z",
        createdAt: "2026-04-11T12:10:00.000Z",
        updatedAt: "2026-04-11T12:10:00.000Z",
      }),
    ).toBe(true);
    expect(store.locks.get("task:123")?.owner).toBe("worker-b");

    store.close();
  });

  test("filters scheduled items when loops are paused and supports retry markers", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_paused",
      seq: 1,
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
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.queue.upsert({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_paused",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      dedupeKey: "worker:loop_paused",
      priority: 3,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "worker:loop_paused",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(store.queue.listScheduled(now)).toHaveLength(0);

    const pausedLoop = store.loops.getById("loop_paused");
    if (!pausedLoop) {
      throw new Error("expected paused loop to exist");
    }

    store.loops.upsert({
      ...pausedLoop,
      status: "queued",
      updatedAt: "2026-04-11T12:00:01.000Z",
    });

    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_worker_1",
    ]);

    store.queue.markRetry({
      id: "queue_worker_1",
      availableAt: "2026-04-11T12:00:30.000Z",
      attempts: 1,
      errorKind: "retryable_after_resume",
      errorMessage: "resume later",
      updatedAt: "2026-04-11T12:00:05.000Z",
    });
    expect(store.queue.getById("queue_worker_1")).toMatchObject({
      status: "queued",
      attempts: 1,
      availableAt: "2026-04-11T12:00:30.000Z",
      lastErrorKind: "retryable_after_resume",
    });

    store.close();
  });

  test("requeues running queue items by loop and clears claims", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
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
      nextRunAt: null,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_2",
      seq: 2,
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:43",
      repo: "acme/looper",
      prNumber: 43,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: now,
      nextRunAt: null,
      createdAt: now,
      updatedAt: now,
    });

    store.queue.upsert({
      id: "queue_running_1",
      projectId: "project_1",
      loopId: "loop_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
      priority: 2,
      status: "running",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: "executor-1",
      claimedAt: now,
      startedAt: now,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });
    store.queue.upsert({
      id: "queue_running_other_loop",
      projectId: "project_1",
      loopId: "loop_2",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:43",
      repo: "acme/looper",
      prNumber: 43,
      dedupeKey: "reviewer:acme/looper:43",
      priority: 2,
      status: "running",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: "executor-2",
      claimedAt: now,
      startedAt: now,
      finishedAt: null,
      lockKey: "pr:acme/looper:43",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(
      store.queue.requeueRunningByLoop("loop_1", "2026-04-11T12:01:00.000Z"),
    ).toBe(1);
    expect(store.queue.getById("queue_running_1")).toMatchObject({
      status: "queued",
      availableAt: "2026-04-11T12:01:00.000Z",
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
    });
    expect(store.queue.getById("queue_running_other_loop")?.status).toBe(
      "running",
    );

    store.close();
  });

  test("lists runs by status in stable newest-first order", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: "2026-04-11T11:59:00.000Z",
      updatedAt: "2026-04-11T11:59:00.000Z",
    });
    store.loops.upsert({
      id: "loop_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: "2026-04-11T12:00:00.000Z",
      createdAt: "2026-04-11T11:59:30.000Z",
      updatedAt: "2026-04-11T11:59:30.000Z",
    });

    store.runs.upsert({
      id: "run_old_running",
      loopId: "loop_1",
      status: "running",
      currentStep: "step_old",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:00:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:00:00.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
    });
    store.runs.upsert({
      id: "run_new_running",
      loopId: "loop_1",
      status: "running",
      currentStep: "step_new",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:05:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:05:00.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:05:00.000Z",
      updatedAt: "2026-04-11T12:05:00.000Z",
    });
    store.runs.upsert({
      id: "run_done",
      loopId: "loop_1",
      status: "completed",
      currentStep: "done",
      lastCompletedStep: "done",
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:10:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:10:00.000Z",
      endedAt: "2026-04-11T12:12:00.000Z",
      createdAt: "2026-04-11T12:10:00.000Z",
      updatedAt: "2026-04-11T12:12:00.000Z",
    });

    const running = store.runs.listByStatus("running");
    expect(running.map((run) => run.id)).toEqual([
      "run_new_running",
      "run_old_running",
    ]);
    expect(running.every((run) => run.status === "running")).toBe(true);

    store.close();
  });
});
