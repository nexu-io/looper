import { randomUUID } from "node:crypto";

import type { Store } from "../storage/store";
import type {
  LockRecord,
  QueueFailureKind,
  QueueItemRecord,
} from "../storage/types";

export const QUEUE_LOOP_PRIORITIES = {
  planner: 1,
  reviewer: 2,
  fixer: 3,
  worker: 4,
} as const;

export type QueueLoopType = keyof typeof QUEUE_LOOP_PRIORITIES;

export interface EnqueueQueueItemInput {
  id?: string;
  projectId?: string | null;
  loopId?: string | null;
  type: QueueLoopType;
  targetType: string;
  targetId: string;
  repo?: string | null;
  prNumber?: number | null;
  dedupeKey: string;
  availableAt?: string;
  maxAttempts?: number;
  lockKey?: string | null;
  payloadJson?: string | null;
}

export interface SchedulerQueueOptions {
  store: Store;
  retryMaxAttempts: number;
  retryBaseDelayMs: number;
  now?: () => Date;
}

export class SchedulerQueue {
  private readonly now: () => Date;

  constructor(private readonly options: SchedulerQueueOptions) {
    this.now = options.now ?? (() => new Date());
  }

  public enqueue(input: EnqueueQueueItemInput): QueueItemRecord {
    const existing = this.options.store.queue.findActiveByDedupe(
      input.dedupeKey,
    );
    if (existing) {
      return existing;
    }

    const nowIso = this.now().toISOString();
    const record: QueueItemRecord = {
      id: input.id ?? randomUUID(),
      projectId: input.projectId ?? null,
      loopId: input.loopId ?? null,
      type: input.type,
      targetType: input.targetType,
      targetId: input.targetId,
      repo: input.repo ?? null,
      prNumber: input.prNumber ?? null,
      dedupeKey: input.dedupeKey,
      priority: getQueuePriority(input.type),
      status: "queued",
      availableAt: input.availableAt ?? nowIso,
      attempts: 0,
      maxAttempts: input.maxAttempts ?? this.options.retryMaxAttempts,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: input.lockKey ?? deriveLockKey(input),
      payloadJson: input.payloadJson ?? null,
      lastError: null,
      lastErrorKind: null,
      createdAt: nowIso,
      updatedAt: nowIso,
    };

    this.options.store.queue.upsert(record);
    return record;
  }

  public listScheduled(limit = 100): QueueItemRecord[] {
    return this.options.store.queue.listScheduled(
      this.now().toISOString(),
      limit,
    );
  }

  public claimNext(claimedBy: string): QueueItemRecord | null {
    return this.options.store.queue.claimNext(
      this.now().toISOString(),
      claimedBy,
    );
  }

  public claimNextOfType(
    claimedBy: string,
    type: QueueLoopType,
  ): QueueItemRecord | null {
    return this.options.store.queue.claimNextOfType(
      this.now().toISOString(),
      claimedBy,
      type,
    );
  }

  public complete(itemId: string): void {
    this.options.store.queue.complete(itemId, this.now().toISOString());
  }

  public fail(
    itemId: string,
    errorKind: QueueFailureKind,
    errorMessage?: string,
  ): QueueItemRecord | null {
    const item = this.options.store.queue.getById(itemId);
    if (!item) {
      return null;
    }

    const nowIso = this.now().toISOString();
    const nextAttempts = item.attempts + 1;

    if (isRetryableFailure(errorKind) && nextAttempts < item.maxAttempts) {
      this.options.store.queue.markRetry({
        id: itemId,
        availableAt: computeNextAttemptAt(
          nowIso,
          this.options.retryBaseDelayMs,
          nextAttempts,
        ),
        attempts: nextAttempts,
        errorMessage,
        errorKind,
        updatedAt: nowIso,
      });
    } else {
      this.options.store.queue.fail({
        id: itemId,
        finishedAt: nowIso,
        errorMessage,
        errorKind,
        updatedAt: nowIso,
      });
    }

    return this.options.store.queue.getById(itemId);
  }

  public cancelByLoop(loopId: string, reason?: string): number {
    return this.options.store.queue.cancelByLoop(
      loopId,
      this.now().toISOString(),
      reason,
    );
  }

  public acquireBusinessLock(
    input: Omit<LockRecord, "createdAt" | "updatedAt">,
  ): boolean {
    const nowIso = this.now().toISOString();
    return this.options.store.locks.acquire({
      ...input,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
  }

  public releaseBusinessLock(key: string): void {
    this.options.store.locks.release(key);
  }
}

export function getQueuePriority(type: QueueLoopType): number {
  return QUEUE_LOOP_PRIORITIES[type];
}

export function computeBackoffDelayMs(
  baseDelayMs: number,
  attempts: number,
): number {
  return baseDelayMs * 2 ** Math.max(0, attempts - 1);
}

export function computeNextAttemptAt(
  nowIso: string,
  baseDelayMs: number,
  attempts: number,
): string {
  return new Date(
    new Date(nowIso).getTime() + computeBackoffDelayMs(baseDelayMs, attempts),
  ).toISOString();
}

export function isRetryableFailure(kind: QueueFailureKind): boolean {
  return kind === "retryable_transient" || kind === "retryable_after_resume";
}

export function deriveLockKey(input: {
  loopId?: string | null;
  repo?: string | null;
  prNumber?: number | null;
}): string | null {
  if (input.repo && input.prNumber) {
    return `pr:${input.repo}:${input.prNumber}`;
  }

  if (input.loopId) {
    return `worker:${input.loopId}`;
  }

  return null;
}
