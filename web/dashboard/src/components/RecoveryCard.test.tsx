import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RecoveryCard } from "@/components/RecoveryCard";
import type { Loop } from "@/lib/api";
import { ToastProvider } from "@/lib/toast";

const fetchLoopWorktree = vi.fn();
const retryLoop = vi.fn();
const stopActiveRun = vi.fn();
const takeoverLoop = vi.fn();

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    fetchLoopWorktree: (...args: unknown[]) => fetchLoopWorktree(...args),
    retryLoop: (...args: unknown[]) => retryLoop(...args),
    stopActiveRun: (...args: unknown[]) => stopActiveRun(...args),
    takeoverLoop: (...args: unknown[]) => takeoverLoop(...args),
  };
});

function baseLoop(overrides?: Partial<Loop>): Loop {
  return {
    id: "loop_1",
    seq: 617,
    projectId: "project_1",
    type: "worker",
    targetType: "project",
    status: "paused",
    displayStatus: "manual_intervention",
    lastFailureKind: "manual_intervention",
    lastFailureReason: "dirty worker worktree: uncommitted local changes",
    createdAt: "2026-04-11T12:00:00.000Z",
    updatedAt: "2026-04-11T12:00:00.000Z",
    ...overrides,
  };
}

function renderCard(loop: Loop = baseLoop()) {
  const onMutated = vi.fn().mockResolvedValue(undefined);
  render(
    <ToastProvider>
      <RecoveryCard
        loop={loop}
        selector={String(loop.seq)}
        onMutated={onMutated}
      />
    </ToastProvider>,
  );
  return { onMutated };
}

describe("RecoveryCard", () => {
  beforeEach(() => {
    fetchLoopWorktree.mockReset();
    retryLoop.mockReset();
    stopActiveRun.mockReset();
    takeoverLoop.mockReset();
    retryLoop.mockResolvedValue({
      loop: { id: "loop_1", status: "queued" },
      mode: "auto",
      resetAttempts: true,
      discardWorktreeChanges: false,
    });
  });

  afterEach(() => {
    cleanup();
  });

  it("renders nothing when displayStatus is not manual_intervention", () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: true,
      managed: true,
      dirty: false,
    });
    renderCard(baseLoop({ displayStatus: "paused", status: "paused" }));
    expect(screen.queryByText(/Manual intervention required/i)).toBeNull();
    expect(fetchLoopWorktree).not.toHaveBeenCalled();
  });

  it("does not render for awaiting_human (decision card owns that path)", () => {
    renderCard(
      baseLoop({
        status: "awaiting_human",
        displayStatus: "awaiting_human",
        lastFailureReason: null,
      }),
    );
    expect(screen.queryByText(/Manual intervention required/i)).toBeNull();
    expect(fetchLoopWorktree).not.toHaveBeenCalled();
  });

  it("shows failure reason and recommends Retry for clean worktree", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: true,
      managed: true,
      dirty: false,
      clean: true,
      worktreePath: "/tmp/clean-wt",
      reason: "already_clean",
    });
    renderCard();

    await screen.findByText(/Manual intervention required/i);
    expect(
      screen.getByText(/dirty worker worktree: uncommitted local changes/i),
    ).toBeTruthy();
    expect(screen.getByText("/tmp/clean-wt")).toBeTruthy();
    expect(screen.getByText("managed")).toBeTruthy();
    expect(screen.getByText("clean")).toBeTruthy();

    // Viewing card loads worktree (GET only) and does not mutate.
    expect(retryLoop).not.toHaveBeenCalled();
    expect(stopActiveRun).not.toHaveBeenCalled();
    expect(takeoverLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: false,
      });
    });
    expect(screen.queryByText("Discard & Retry")).toBeNull();
  });

  it("offers Inspect/Jump and confirmed Discard & Retry for managed dirty", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/dirty-wt",
      branch: "feat/x",
      reason: "dirty",
    });
    renderCard();

    await screen.findByText(/Managed worktree has local uncommitted changes/i);
    fireEvent.click(screen.getByRole("button", { name: "Inspect / Jump" }));
    await screen.findByText(/Inspect dirty worktree/i);
    expect(screen.getByText("looper jump 617")).toBeTruthy();
    expect(screen.getAllByText("/tmp/dirty-wt").length).toBeGreaterThan(0);
    // Inspect does not mutate.
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    fireEvent.click(screen.getByRole("button", { name: "Discard & Retry" }));
    await screen.findByText(/Discard worktree changes and retry/i);
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Discard & retry" }));
    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: true,
      });
    });
  });

  it("never offers discard for unmanaged dirty worktree", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: true,
      managed: false,
      dirty: true,
      worktreePath: "/tmp/primary-repo",
      reason: "unmanaged",
    });
    renderCard();

    await screen.findByText(/Dashboard discard is unavailable/i);
    expect(screen.queryByRole("button", { name: "Discard & Retry" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
    await screen.findByText(/Inspect worktree/i);
    expect(
      screen.getAllByText(/Discard is unavailable/i).length,
    ).toBeGreaterThan(0);
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("unclassifiable mode shows reason/logs without guessing repair", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: false,
      managed: false,
      reason: "no_worktree",
    });
    renderCard(
      baseLoop({
        lastFailureReason: "checkpoint hold: operator must inspect",
      }),
    );

    await screen.findByText(/no safe worktree repair path/i);
    expect(
      screen.getByText(/checkpoint hold: operator must inspect/i),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "View logs" })).toBeTruthy();
    expect(screen.queryByText("Discard & Retry")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(retryLoop).not.toHaveBeenCalled();
  });
});
