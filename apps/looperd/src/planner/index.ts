import type { SchedulerQueue } from "../scheduler/index";
import type { Store } from "../storage/store";
import type { QueueFailureKind, QueueItemRecord } from "../storage/types";

export interface PlannerLoopRunnerOptions {
  store: Store;
  scheduler: SchedulerQueue;
}

export interface DiscoverIssuesResult {
  queueItems: QueueItemRecord[];
  createdLoopIds: string[];
  skipped: number;
}

export interface PlannerClaimedItemResult {
  loopId: string;
  runId: string;
  queueItemId: string;
  status: "success" | "failed";
  summary: string;
  failureKind?: QueueFailureKind;
}

export class PlannerLoopRunner {
  public async discoverIssues(): Promise<DiscoverIssuesResult> {
    return {
      queueItems: [],
      createdLoopIds: [],
      skipped: 0,
    };
  }

  public async processClaimedItem(
    queueItem: QueueItemRecord,
  ): Promise<PlannerClaimedItemResult> {
    if (queueItem.type !== "planner" || !queueItem.loopId) {
      throw new Error("planner queue item is missing loop context");
    }

    return {
      loopId: queueItem.loopId,
      runId: queueItem.loopId,
      queueItemId: queueItem.id,
      status: "failed",
      failureKind: "non_retryable",
      summary: "Planner loop runner is not implemented yet",
    };
  }
}
