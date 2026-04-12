import { describe, expect, test } from "bun:test";

import {
  assertLoopStatusTransition,
  assertRunStatusTransition,
  assertStepBelongsToLoopType,
  assertUniqueActiveLoop,
  createAuditEvent,
  createLock,
  createLoop,
  createPrLockKey,
  createProject,
  createRun,
  defineIssueLoopTarget,
  defineProjectLoopTarget,
  definePullRequestLoopTarget,
} from "./index";

const now = "2026-04-11T12:00:00.000Z";

describe("domain invariants", () => {
  test("creates a valid worker loop for a project target", () => {
    const loop = createLoop({
      id: "loop_worker_1",
      projectId: "project_1",
      type: "worker",
      target: defineProjectLoopTarget("project_1"),
      status: "idle",
      createdAt: now,
      updatedAt: now,
    });

    expect(loop.target).toEqual({
      targetType: "project",
      projectId: "project_1",
    });
  });

  test("creates a valid planner loop for an issue target", () => {
    const loop = createLoop({
      id: "loop_planner_1",
      projectId: "project_1",
      type: "planner",
      target: defineIssueLoopTarget("acme/looper", 123),
      status: "idle",
      createdAt: now,
      updatedAt: now,
    });

    expect(loop.target).toEqual({
      targetType: "issue",
      repo: "acme/looper",
      issueNumber: 123,
    });
  });

  test("rejects reviewer loops that do not target a pull request", () => {
    expect(() =>
      createLoop({
        id: "loop_reviewer_1",
        projectId: "project_1",
        type: "reviewer",
        target: defineProjectLoopTarget("project_1"),
        status: "idle",
        createdAt: now,
        updatedAt: now,
      }),
    ).toThrow("reviewer loops must target a pull request");
  });

  test("allows worker loops to target an existing pull request", () => {
    expect(() =>
      createLoop({
        id: "loop_worker_pr_1",
        projectId: "project_1",
        type: "worker",
        target: definePullRequestLoopTarget("acme/looper", 42),
        status: "idle",
        createdAt: now,
        updatedAt: now,
      }),
    ).not.toThrow();
  });

  test("rejects duplicate active loops for the same project, type, and target", () => {
    expect(() =>
      assertUniqueActiveLoop({
        loops: [
          {
            id: "loop_existing",
            projectId: "project_1",
            type: "reviewer",
            target: definePullRequestLoopTarget("acme/looper", 42),
            status: "running",
          },
        ],
        candidate: {
          id: "loop_candidate",
          projectId: "project_1",
          type: "reviewer",
          target: definePullRequestLoopTarget("acme/looper", 42),
          status: "queued",
        },
      }),
    ).toThrow("active loop already exists");
  });

  test("allows terminal loops to coexist with a new active loop", () => {
    expect(() =>
      assertUniqueActiveLoop({
        loops: [
          {
            id: "loop_completed",
            projectId: "project_1",
            type: "reviewer",
            target: definePullRequestLoopTarget("acme/looper", 42),
            status: "completed",
          },
        ],
        candidate: {
          id: "loop_candidate",
          projectId: "project_1",
          type: "reviewer",
          target: definePullRequestLoopTarget("acme/looper", 42),
          status: "queued",
        },
      }),
    ).not.toThrow();
  });

  test("enforces loop and run transition rules", () => {
    expect(() => assertLoopStatusTransition("idle", "queued")).not.toThrow();
    expect(() => assertLoopStatusTransition("queued", "completed")).toThrow(
      "invalid loop status transition",
    );

    expect(() => assertRunStatusTransition("queued", "running")).not.toThrow();
    expect(() => assertRunStatusTransition("success", "failed")).toThrow(
      "invalid run status transition",
    );
  });

  test("enforces endedAt only on terminal run statuses", () => {
    expect(() =>
      createRun({
        id: "run_1",
        loopId: "loop_1",
        status: "running",
        startedAt: now,
        endedAt: now,
        createdAt: now,
        updatedAt: now,
      }),
    ).toThrow("only terminal runs may define endedAt");
  });

  test("rejects run steps that do not match loop type when provided", () => {
    expect(() =>
      createRun({
        id: "run_2",
        loopId: "loop_1",
        loopType: "worker",
        status: "running",
        currentStep: "review",
        startedAt: now,
        createdAt: now,
        updatedAt: now,
      }),
    ).toThrow("does not belong to loop type worker");

    expect(() =>
      assertStepBelongsToLoopType("reviewer", "review"),
    ).not.toThrow();

    expect(() =>
      assertStepBelongsToLoopType("planner", "write-spec"),
    ).not.toThrow();
  });

  test("builds audit event envelopes and lock keys", () => {
    const event = createAuditEvent({
      id: "event_1",
      eventType: "loop.step.completed",
      entity: {
        entityType: "loop",
        entityId: "loop_1",
      },
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      payload: {
        step: "review",
        durationMs: 1200,
      },
      createdAt: now,
    });

    expect(event.payload.step).toBe("review");
    expect(createPrLockKey("acme/looper", 42)).toBe("pr:acme/looper:42");
  });

  test("creates projects and validates lock expiry ordering", () => {
    const project = createProject({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      archived: false,
      createdAt: now,
      updatedAt: now,
    });

    expect(project.name).toBe("Looper");

    expect(() =>
      createLock({
        key: "task:task_1",
        owner: "worker",
        expiresAt: now,
        createdAt: now,
        updatedAt: now,
      }),
    ).toThrow("lock.expiresAt must be after lock.createdAt");
  });
});
