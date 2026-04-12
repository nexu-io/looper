export const LOOP_TYPES = ["planner", "reviewer", "worker", "fixer"] as const;
export type LoopType = (typeof LOOP_TYPES)[number];

export const LOOP_TARGET_TYPES = ["project", "pull_request", "issue"] as const;
export type LoopTargetType = (typeof LOOP_TARGET_TYPES)[number];

export const LOOP_STATUSES = [
  "idle",
  "queued",
  "running",
  "paused",
  "completed",
  "failed",
  "interrupted",
] as const;
export type LoopStatus = (typeof LOOP_STATUSES)[number];

export const RUN_STATUSES = [
  "queued",
  "running",
  "success",
  "failed",
  "cancelled",
  "interrupted",
  "parse_failed",
] as const;
export type RunStatus = (typeof RUN_STATUSES)[number];

export const PLANNER_STEPS = [
  "discover-issues",
  "prepare-worktree",
  "write-spec",
  "publish",
  "notify",
] as const;
export type PlannerStep = (typeof PLANNER_STEPS)[number];

export const REVIEWER_STEPS = [
  "discover",
  "filter",
  "claim",
  "snapshot",
  "review",
  "publish",
] as const;
export type ReviewerStep = (typeof REVIEWER_STEPS)[number];

export const WORKER_STEPS = [
  "prepare-work",
  "prepare-worktree",
  "plan",
  "execute",
  "validate",
  "open-pr",
] as const;
export type WorkerStep = (typeof WORKER_STEPS)[number];

export const FIXER_STEPS = [
  "discover-pr",
  "claim-pr",
  "collect-fixes",
  "prepare-worktree",
  "repair",
  "validate",
  "push",
  "reconcile-commits",
  "resolve-comments",
  "recheck",
] as const;
export type FixerStep = (typeof FIXER_STEPS)[number];

export type LoopStep = PlannerStep | ReviewerStep | WorkerStep | FixerStep;

const ALL_LOOP_STEPS = [
  ...PLANNER_STEPS,
  ...REVIEWER_STEPS,
  ...WORKER_STEPS,
  ...FIXER_STEPS,
] as const;

const LOOP_STEPS_BY_TYPE: Readonly<Record<LoopType, readonly LoopStep[]>> = {
  planner: PLANNER_STEPS,
  reviewer: REVIEWER_STEPS,
  worker: WORKER_STEPS,
  fixer: FIXER_STEPS,
};

export const ACTIVE_LOOP_STATUSES = [
  "idle",
  "queued",
  "running",
  "paused",
] as const;
export type ActiveLoopStatus = (typeof ACTIVE_LOOP_STATUSES)[number];

export const TERMINAL_LOOP_STATUSES = [
  "completed",
  "failed",
  "interrupted",
] as const;
export type TerminalLoopStatus = (typeof TERMINAL_LOOP_STATUSES)[number];

export const TERMINAL_RUN_STATUSES = [
  "success",
  "failed",
  "cancelled",
  "interrupted",
  "parse_failed",
] as const;
export type TerminalRunStatus = (typeof TERMINAL_RUN_STATUSES)[number];

export const RESUME_POLICIES = [
  "replay_step",
  "advance_from_checkpoint",
  "manual_intervention",
] as const;
export type ResumePolicy = (typeof RESUME_POLICIES)[number];

export const AUDIT_EVENT_TYPES = [
  "loop.created",
  "loop.started",
  "loop.step.started",
  "loop.step.completed",
  "loop.step.failed",
  "run.started",
  "run.completed",
  "run.failed",
  "run.cancelled",
  "agent.invoked",
  "agent.heartbeat",
  "agent.completed",
  "agent.timed_out",
  "agent.killed",
  "pr.review.posted",
  "pr.branch.pushed",
  "notification.sent",
] as const;
export type AuditEventType = (typeof AUDIT_EVENT_TYPES)[number];

export const AUDIT_ENTITY_TYPES = [
  "project",
  "loop",
  "run",
  "pull_request",
  "lock",
  "notification",
  "agent_execution",
] as const;
export type AuditEntityType = (typeof AUDIT_ENTITY_TYPES)[number];

export interface Project {
  id: string;
  name: string;
  repoPath: string;
  baseBranch?: string;
  archived: boolean;
  metadata?: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
}

export interface ProjectLoopTarget {
  targetType: "project";
  projectId: string;
}

export interface PullRequestLoopTarget {
  targetType: "pull_request";
  repo: string;
  prNumber: number;
}

export interface IssueLoopTarget {
  targetType: "issue";
  repo: string;
  issueNumber: number;
}

export type LoopTarget =
  | ProjectLoopTarget
  | PullRequestLoopTarget
  | IssueLoopTarget;

export interface Loop {
  id: string;
  projectId: string;
  type: LoopType;
  target: LoopTarget;
  status: LoopStatus;
  config?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
  lastRunAt?: string;
  nextRunAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface Run {
  id: string;
  loopId: string;
  status: RunStatus;
  currentStep?: LoopStep;
  lastCompletedStep?: LoopStep;
  checkpoint?: Record<string, unknown>;
  summary?: string;
  errorMessage?: string;
  startedAt: string;
  lastHeartbeatAt?: string;
  endedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface PullRequestSnapshot {
  id: string;
  projectId: string;
  repo: string;
  prNumber: number;
  headSha: string;
  baseSha?: string;
  title: string;
  body?: string;
  author: string;
  diffRef?: string;
  checksSummary?: string;
  unresolvedThreadCount: number;
  reviewState?: string;
  payload?: Record<string, unknown>;
  capturedAt: string;
  createdAt: string;
}

export interface Lock {
  key: string;
  owner: string;
  reason?: string;
  expiresAt: string;
  createdAt: string;
  updatedAt: string;
}

export interface AuditEntityRef {
  entityType: AuditEntityType;
  entityId: string;
}

export interface AuditActor {
  type: "system" | "user" | "agent";
  id: string;
  displayName?: string;
}

export interface AuditEvent<
  TPayload extends Record<string, unknown> = Record<string, unknown>,
> {
  id: string;
  eventType: AuditEventType;
  entity: AuditEntityRef;
  projectId?: string;
  loopId?: string;
  runId?: string;
  actor?: AuditActor;
  correlationId?: string;
  causationId?: string;
  payload: TPayload;
  createdAt: string;
}

export type CreateProjectInput = Project;

export type CreateLoopInput = Omit<Loop, "target"> & {
  target: LoopTarget;
};

export type CreateRunInput = Run & { loopType?: LoopType };

export type CreatePullRequestSnapshotInput = PullRequestSnapshot;

export type CreateLockInput = Lock;

export type CreateAuditEventInput<
  TPayload extends Record<string, unknown> = Record<string, unknown>,
> = AuditEvent<TPayload>;

const LOOP_STATUS_TRANSITIONS: Readonly<
  Record<LoopStatus, readonly LoopStatus[]>
> = {
  idle: ["queued"],
  queued: ["running"],
  running: ["completed", "failed", "paused", "interrupted"],
  paused: ["queued", "completed"],
  completed: [],
  failed: [],
  interrupted: ["queued", "failed"],
};

const RUN_STATUS_TRANSITIONS: Readonly<
  Record<RunStatus, readonly RunStatus[]>
> = {
  queued: ["running"],
  running: TERMINAL_RUN_STATUSES,
  success: [],
  failed: [],
  cancelled: [],
  interrupted: [],
  parse_failed: [],
};

function assertNonEmpty(value: string, fieldName: string): void {
  if (value.trim().length === 0) {
    throw new Error(`${fieldName} must not be empty`);
  }
}

function assertPositiveInteger(value: number, fieldName: string): void {
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${fieldName} must be a positive integer`);
  }
}

function assertTimestamp(value: string, fieldName: string): void {
  if (Number.isNaN(Date.parse(value))) {
    throw new Error(`${fieldName} must be a valid ISO timestamp`);
  }
}

function assertKnownValue<T extends string>(
  value: T,
  allowed: readonly T[],
  fieldName: string,
): void {
  if (!allowed.includes(value)) {
    throw new Error(`${fieldName} must be one of: ${allowed.join(", ")}`);
  }
}

export function isActiveLoopStatus(
  status: LoopStatus,
): status is ActiveLoopStatus {
  return ACTIVE_LOOP_STATUSES.includes(status as ActiveLoopStatus);
}

export function isTerminalRunStatus(
  status: RunStatus,
): status is TerminalRunStatus {
  return TERMINAL_RUN_STATUSES.includes(status as TerminalRunStatus);
}

export function defineProjectLoopTarget(projectId: string): ProjectLoopTarget {
  assertNonEmpty(projectId, "target.projectId");

  return {
    targetType: "project",
    projectId,
  };
}

export function definePullRequestLoopTarget(
  repo: string,
  prNumber: number,
): PullRequestLoopTarget {
  assertNonEmpty(repo, "target.repo");
  assertPositiveInteger(prNumber, "target.prNumber");

  return {
    targetType: "pull_request",
    repo,
    prNumber,
  };
}

export function defineIssueLoopTarget(
  repo: string,
  issueNumber: number,
): IssueLoopTarget {
  assertNonEmpty(repo, "target.repo");
  assertPositiveInteger(issueNumber, "target.issueNumber");

  return {
    targetType: "issue",
    repo,
    issueNumber,
  };
}

export function assertLoopTypeMatchesTarget(
  loopType: LoopType,
  target: LoopTarget,
): void {
  if (
    loopType === "worker" &&
    target.targetType !== "project" &&
    target.targetType !== "pull_request"
  ) {
    throw new Error("worker loops must target a project or pull request");
  }

  if (loopType === "planner" && target.targetType !== "issue") {
    throw new Error("planner loops must target an issue");
  }

  if (
    (loopType === "reviewer" || loopType === "fixer") &&
    target.targetType !== "pull_request"
  ) {
    throw new Error(`${loopType} loops must target a pull request`);
  }
}

export function assertUniqueActiveLoop(inputs: {
  loops: readonly Pick<
    Loop,
    "id" | "projectId" | "type" | "target" | "status"
  >[];
  candidate: Pick<Loop, "id" | "projectId" | "type" | "target" | "status">;
}): void {
  if (!isActiveLoopStatus(inputs.candidate.status)) {
    return;
  }

  const conflict = inputs.loops.find((loop) => {
    if (loop.id === inputs.candidate.id || !isActiveLoopStatus(loop.status)) {
      return false;
    }

    return (
      loop.projectId === inputs.candidate.projectId &&
      loop.type === inputs.candidate.type &&
      getLoopTargetKey(loop.target) ===
        getLoopTargetKey(inputs.candidate.target)
    );
  });

  if (conflict) {
    throw new Error(
      `active loop already exists for ${inputs.candidate.projectId}:${inputs.candidate.type}:${getLoopTargetKey(inputs.candidate.target)}`,
    );
  }
}

export function assertLoopStatusTransition(
  from: LoopStatus,
  to: LoopStatus,
): void {
  assertKnownValue(from, LOOP_STATUSES, "fromStatus");
  assertKnownValue(to, LOOP_STATUSES, "toStatus");

  if (!LOOP_STATUS_TRANSITIONS[from].includes(to)) {
    throw new Error(`invalid loop status transition: ${from} -> ${to}`);
  }
}

export function assertRunStatusTransition(
  from: RunStatus,
  to: RunStatus,
): void {
  assertKnownValue(from, RUN_STATUSES, "fromStatus");
  assertKnownValue(to, RUN_STATUSES, "toStatus");

  if (!RUN_STATUS_TRANSITIONS[from].includes(to)) {
    throw new Error(`invalid run status transition: ${from} -> ${to}`);
  }
}

export function assertStepBelongsToLoopType(
  loopType: LoopType,
  step: LoopStep,
): void {
  assertKnownValue(loopType, LOOP_TYPES, "loopType");
  assertKnownValue(step, ALL_LOOP_STEPS, "step");

  if (!LOOP_STEPS_BY_TYPE[loopType].includes(step)) {
    throw new Error(`step ${step} does not belong to loop type ${loopType}`);
  }
}

export function getLoopTargetKey(target: LoopTarget): string {
  if (target.targetType === "project") {
    return `project:${target.projectId}`;
  }

  if (target.targetType === "issue") {
    return `issue:${target.repo}:${target.issueNumber}`;
  }

  return `pull_request:${target.repo}:${target.prNumber}`;
}

export function createPrLockKey(repo: string, prNumber: number): string {
  assertNonEmpty(repo, "repo");
  assertPositiveInteger(prNumber, "prNumber");

  return `pr:${repo}:${prNumber}`;
}

export function createProject(input: CreateProjectInput): Project {
  assertNonEmpty(input.id, "project.id");
  assertNonEmpty(input.name, "project.name");
  assertNonEmpty(input.repoPath, "project.repoPath");
  assertTimestamp(input.createdAt, "project.createdAt");
  assertTimestamp(input.updatedAt, "project.updatedAt");

  if (input.baseBranch !== undefined) {
    assertNonEmpty(input.baseBranch, "project.baseBranch");
  }

  return { ...input };
}

export function createLoop(input: CreateLoopInput): Loop {
  assertNonEmpty(input.id, "loop.id");
  assertNonEmpty(input.projectId, "loop.projectId");
  assertKnownValue(input.type, LOOP_TYPES, "loop.type");
  assertKnownValue(input.status, LOOP_STATUSES, "loop.status");
  assertTimestamp(input.createdAt, "loop.createdAt");
  assertTimestamp(input.updatedAt, "loop.updatedAt");
  assertLoopTypeMatchesTarget(input.type, input.target);

  if (input.lastRunAt) {
    assertTimestamp(input.lastRunAt, "loop.lastRunAt");
  }

  if (input.nextRunAt) {
    assertTimestamp(input.nextRunAt, "loop.nextRunAt");
  }

  return { ...input };
}

export function createRun(input: CreateRunInput): Run {
  assertNonEmpty(input.id, "run.id");
  assertNonEmpty(input.loopId, "run.loopId");
  assertKnownValue(input.status, RUN_STATUSES, "run.status");
  assertTimestamp(input.startedAt, "run.startedAt");
  assertTimestamp(input.createdAt, "run.createdAt");
  assertTimestamp(input.updatedAt, "run.updatedAt");

  if (input.currentStep) {
    assertKnownValue(input.currentStep, ALL_LOOP_STEPS, "run.currentStep");

    if (input.loopType) {
      assertStepBelongsToLoopType(input.loopType, input.currentStep);
    }
  }

  if (input.lastCompletedStep) {
    assertKnownValue(
      input.lastCompletedStep,
      ALL_LOOP_STEPS,
      "run.lastCompletedStep",
    );

    if (input.loopType) {
      assertStepBelongsToLoopType(input.loopType, input.lastCompletedStep);
    }
  }

  if (input.lastHeartbeatAt) {
    assertTimestamp(input.lastHeartbeatAt, "run.lastHeartbeatAt");
  }

  if (input.endedAt) {
    assertTimestamp(input.endedAt, "run.endedAt");

    if (!isTerminalRunStatus(input.status)) {
      throw new Error("only terminal runs may define endedAt");
    }
  }

  return { ...input };
}

export function createPullRequestSnapshot(
  input: CreatePullRequestSnapshotInput,
): PullRequestSnapshot {
  assertNonEmpty(input.id, "pullRequestSnapshot.id");
  assertNonEmpty(input.projectId, "pullRequestSnapshot.projectId");
  assertNonEmpty(input.repo, "pullRequestSnapshot.repo");
  assertPositiveInteger(input.prNumber, "pullRequestSnapshot.prNumber");
  assertNonEmpty(input.headSha, "pullRequestSnapshot.headSha");
  assertNonEmpty(input.title, "pullRequestSnapshot.title");
  assertNonEmpty(input.author, "pullRequestSnapshot.author");
  assertTimestamp(input.capturedAt, "pullRequestSnapshot.capturedAt");
  assertTimestamp(input.createdAt, "pullRequestSnapshot.createdAt");

  if (
    !Number.isInteger(input.unresolvedThreadCount) ||
    input.unresolvedThreadCount < 0
  ) {
    throw new Error(
      "pullRequestSnapshot.unresolvedThreadCount must be a non-negative integer",
    );
  }

  return { ...input };
}

export function createLock(input: CreateLockInput): Lock {
  assertNonEmpty(input.key, "lock.key");
  assertNonEmpty(input.owner, "lock.owner");
  assertTimestamp(input.expiresAt, "lock.expiresAt");
  assertTimestamp(input.createdAt, "lock.createdAt");
  assertTimestamp(input.updatedAt, "lock.updatedAt");

  if (input.expiresAt <= input.createdAt) {
    throw new Error("lock.expiresAt must be after lock.createdAt");
  }

  return { ...input };
}

export function createAuditEvent<
  TPayload extends Record<string, unknown> = Record<string, unknown>,
>(input: CreateAuditEventInput<TPayload>): AuditEvent<TPayload> {
  assertNonEmpty(input.id, "auditEvent.id");
  assertKnownValue(input.eventType, AUDIT_EVENT_TYPES, "auditEvent.eventType");
  assertKnownValue(
    input.entity.entityType,
    AUDIT_ENTITY_TYPES,
    "auditEvent.entity.entityType",
  );
  assertNonEmpty(input.entity.entityId, "auditEvent.entity.entityId");
  assertTimestamp(input.createdAt, "auditEvent.createdAt");

  if (input.projectId) {
    assertNonEmpty(input.projectId, "auditEvent.projectId");
  }

  if (input.loopId) {
    assertNonEmpty(input.loopId, "auditEvent.loopId");
  }

  if (input.runId) {
    assertNonEmpty(input.runId, "auditEvent.runId");
  }

  if (input.actor) {
    assertNonEmpty(input.actor.id, "auditEvent.actor.id");
  }

  return {
    ...input,
    payload: { ...input.payload },
  };
}
