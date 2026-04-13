import { describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { SqliteStore } from "../storage/sqlite/sqlite-store";
import {
  SchedulerQueue,
  computeBackoffDelayMs,
  computeNextAttemptAt,
  getQueuePriority,
} from "./index";

async function createStore() {
  const rootDir = await mkdtemp(join(tmpdir(), "looper-scheduler-"));
  const store = new SqliteStore({ dbPath: join(rootDir, "looper.sqlite") });
  store.initialize({ autoMigrate: true });
  return { rootDir, store };
}

describe("SchedulerQueue", () => {
  test("dedupes active queue items and retries with exponential backoff", async () => {
    const { rootDir, store } = await createStore();

    try {
      const now = new Date("2026-04-11T12:00:00.000Z");
      const queue = new SchedulerQueue({
        store,
        retryMaxAttempts: 3,
        retryBaseDelayMs: 5_000,
        now: () => now,
      });

      const first = queue.enqueue({
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        dedupeKey: "reviewer:acme/looper:42",
      });
      const duplicate = queue.enqueue({
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        dedupeKey: "reviewer:acme/looper:42",
      });

      expect(duplicate.id).toBe(first.id);
      expect(store.queue.list()).toHaveLength(1);

      const claimed = queue.claimNext("executor-1");
      expect(claimed?.status).toBe("running");

      const retried = queue.fail(
        first.id,
        "retryable_transient",
        "temporary network failure",
      );

      expect(retried?.status).toBe("queued");
      expect(retried?.attempts).toBe(1);
      expect(retried?.availableAt).toBe("2026-04-11T12:00:05.000Z");
      expect(retried?.lastErrorKind).toBe("retryable_transient");

      now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
      queue.claimNext("executor-1");
      const terminal = queue.fail(
        first.id,
        "retryable_transient",
        "still failing",
      );

      expect(terminal?.status).toBe("queued");
      expect(terminal?.attempts).toBe(2);
      expect(terminal?.availableAt).toBe("2026-04-11T12:00:15.000Z");

      now.setTime(new Date("2026-04-11T12:00:15.000Z").getTime());
      queue.claimNext("executor-1");
      const exhausted = queue.fail(
        first.id,
        "retryable_transient",
        "final failure",
      );

      expect(exhausted?.status).toBe("failed");
      expect(exhausted?.finishedAt).toBe("2026-04-11T12:00:15.000Z");
    } finally {
      store.close();
      await rm(rootDir, { recursive: true, force: true });
    }
  });

  test("cancels queued work and manages business locks", async () => {
    const { rootDir, store } = await createStore();

    try {
      const queue = new SchedulerQueue({
        store,
        retryMaxAttempts: 3,
        retryBaseDelayMs: 5_000,
        now: () => new Date("2026-04-11T13:00:00.000Z"),
      });

      store.projects.upsert({
        id: "project_1",
        name: "Looper",
        repoPath: "/tmp/looper",
        baseBranch: "main",
        archived: false,
        metadataJson: null,
        createdAt: "2026-04-11T13:00:00.000Z",
        updatedAt: "2026-04-11T13:00:00.000Z",
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
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: "2026-04-11T13:00:00.000Z",
        createdAt: "2026-04-11T13:00:00.000Z",
        updatedAt: "2026-04-11T13:00:00.000Z",
      });
      const item = queue.enqueue({
        loopId: "loop_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        dedupeKey: "worker:loop_1",
      });

      expect(
        queue.acquireBusinessLock({
          key: "worker:loop_1",
          owner: item.id,
          expiresAt: "2026-04-11T13:05:00.000Z",
          reason: "execute",
        }),
      ).toBe(true);
      expect(store.locks.get("worker:loop_1")?.owner).toBe(item.id);

      expect(queue.cancelByLoop("loop_1", "loop paused")).toBe(1);
      expect(store.queue.getById(item.id)?.status).toBe("cancelled");

      queue.releaseBusinessLock("worker:loop_1");
      expect(store.locks.get("worker:loop_1")).toBeNull();
    } finally {
      store.close();
      await rm(rootDir, { recursive: true, force: true });
    }
  });
});

describe("scheduler backoff helpers", () => {
  test("prioritizes planner work ahead of reviewer, fixer, and worker loops", () => {
    expect(getQueuePriority("planner")).toBeLessThan(
      getQueuePriority("reviewer"),
    );
    expect(getQueuePriority("reviewer")).toBeLessThan(
      getQueuePriority("fixer"),
    );
    expect(getQueuePriority("fixer")).toBeLessThan(getQueuePriority("worker"));
  });

  test("computes exponential retry delays", () => {
    expect(computeBackoffDelayMs(5_000, 1)).toBe(5_000);
    expect(computeBackoffDelayMs(5_000, 2)).toBe(10_000);
    expect(computeBackoffDelayMs(5_000, 3)).toBe(20_000);
    expect(computeNextAttemptAt("2026-04-11T12:00:00.000Z", 5_000, 3)).toBe(
      "2026-04-11T12:00:20.000Z",
    );
  });
});
