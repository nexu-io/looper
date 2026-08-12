import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
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
    expect(fetchLoopWorktree).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: false,
      });
    });
    // Click-time revalidation before plain retry (LoopActionBar parity).
    expect(fetchLoopWorktree).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Discard & Retry")).toBeNull();
  });

  it("blocks plain retry when revalidation finds managed dirty worktree", async () => {
    fetchLoopWorktree
      .mockResolvedValueOnce({
        loopId: "loop_1",
        seq: 617,
        present: true,
        managed: true,
        dirty: false,
        clean: true,
        worktreePath: "/tmp/was-clean",
        reason: "already_clean",
      })
      .mockResolvedValueOnce({
        loopId: "loop_1",
        seq: 617,
        present: true,
        managed: true,
        dirty: true,
        clean: false,
        worktreePath: "/tmp/now-dirty",
        reason: "dirty",
      });
    renderCard();

    await screen.findByRole("button", { name: "Retry" });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(fetchLoopWorktree).toHaveBeenCalledTimes(2);
    });
    expect(retryLoop).not.toHaveBeenCalled();
    await screen.findByText(/Managed worktree has local uncommitted changes/i);
    expect(screen.getByRole("button", { name: "Discard & Retry" })).toBeTruthy();
  });

  it("blocks Retry without discard when revalidation stays inspect-only", async () => {
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

    await screen.findByRole("button", { name: "Retry without discard" });
    fireEvent.click(screen.getByRole("button", { name: "Retry without discard" }));

    await waitFor(() => {
      expect(fetchLoopWorktree).toHaveBeenCalledTimes(2);
    });
    expect(retryLoop).not.toHaveBeenCalled();
    await screen.findByText(/Dashboard discard is unavailable/i);
  });

  it("allows Retry without discard only after revalidation returns ok", async () => {
    fetchLoopWorktree
      .mockResolvedValueOnce({
        loopId: "loop_1",
        seq: 617,
        present: true,
        managed: true,
        dirty: true,
        clean: false,
        worktreePath: "/tmp/dirty-wt",
        reason: "dirty",
      })
      .mockResolvedValueOnce({
        loopId: "loop_1",
        seq: 617,
        present: true,
        managed: true,
        dirty: false,
        clean: true,
        worktreePath: "/tmp/dirty-wt",
        reason: "already_clean",
      });
    renderCard();

    await screen.findByRole("button", { name: "Retry without discard" });
    fireEvent.click(screen.getByRole("button", { name: "Retry without discard" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: false,
      });
    });
    expect(fetchLoopWorktree).toHaveBeenCalledTimes(2);
  });

  it("offers Clear unusable path & Retry for managed unusable_path", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: true,
      managed: true,
      reason: "unusable_path",
      worktreePath: "/tmp/hollow-wt",
      supportsClearUnusablePath: true,
    });
    renderCard(
      baseLoop({
        lastFailureReason:
          "worktree path /tmp/hollow-wt is unusable and not empty; manual intervention required: unusable worktree path preserved",
      }),
    );

    await screen.findByText(/could not verify this managed path as a usable checkout/i);
    expect(screen.getByText("unusable")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Discard & Retry" })).toBeNull();
    fireEvent.click(
      screen.getByRole("button", { name: "Clear unusable path & Retry" }),
    );
    await screen.findByText(/Clear unusable worktree path and retry/i);
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Clear path & retry" }));
    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: false,
        clearUnusableWorktreePath: true,
        expectedWorktreePath: "/tmp/hollow-wt",
      });
    });
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

  it("recommends plain retry when worktree was never created", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 617,
      present: false,
      managed: false,
      reason: "no_worktree",
    });
    renderCard(
      baseLoop({
        lastFailureReason: "prepare failed before worktree creation",
      }),
    );

    await screen.findByText(/Plain retry is safe/i);
    expect(
      screen.getByText(/prepare failed before worktree creation/i),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByText("Discard & Retry")).toBeNull();
    expect(screen.queryByRole("button", { name: "View logs" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("617", {
        discardWorktreeChanges: false,
      });
    });
  });

  it("unclassifiable mode shows reason/logs when preflight fetch fails", async () => {
    fetchLoopWorktree.mockRejectedValue(new Error("worktree endpoint down"));
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

  it("requires confirmation before Stop mutates the active run", async () => {
    fetchLoopWorktree.mockRejectedValue(new Error("worktree endpoint down"));
    stopActiveRun.mockResolvedValue({ ok: true });
    render(
      <ToastProvider>
        <RecoveryCard
          loop={baseLoop({ status: "running" })}
          selector="617"
          hasActiveRun
        />
      </ToastProvider>,
    );

    await screen.findByText(/no safe worktree repair path/i);
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    expect(stopActiveRun).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Stop active run/i)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop" }));
    await waitFor(() => {
      expect(stopActiveRun).toHaveBeenCalledWith("617");
    });
  });

  it("requires confirmation before Takeover parks a running loop", async () => {
    fetchLoopWorktree.mockRejectedValue(new Error("worktree endpoint down"));
    takeoverLoop.mockResolvedValue({
      loopId: "loop_1",
      supported: true,
      worktreePath: "/tmp/wt",
      resumeCommand: "looper handback 617",
    });
    render(
      <ToastProvider>
        <RecoveryCard
          loop={baseLoop({ status: "running" })}
          selector="617"
          hasActiveRun
        />
      </ToastProvider>,
    );

    await screen.findByText(/no safe worktree repair path/i);
    fireEvent.click(screen.getByRole("button", { name: "Takeover" }));
    expect(takeoverLoop).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Take over loop/i)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Takeover" }));
    await waitFor(() => {
      expect(takeoverLoop).toHaveBeenCalledWith("617");
    });
  });

  it("keeps takeover result dialog after refresh flips to human_takeover", async () => {
    fetchLoopWorktree.mockRejectedValue(new Error("worktree endpoint down"));
    takeoverLoop.mockResolvedValue({
      loopId: "loop_1",
      supported: true,
      worktreePath: "/tmp/takeover-wt",
      resumeCommand: "codex resume abc",
      message: "Loop parked for interactive work",
    });
    const onMutated = vi.fn().mockResolvedValue(undefined);

    const { rerender } = render(
      <ToastProvider>
        <RecoveryCard
          loop={baseLoop({ status: "running" })}
          selector="617"
          hasActiveRun
          onMutated={onMutated}
        />
      </ToastProvider>,
    );

    await screen.findByText(/no safe worktree repair path/i);
    fireEvent.click(screen.getByRole("button", { name: "Takeover" }));
    const confirm = await screen.findByRole("dialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Takeover" }));

    await waitFor(() => {
      expect(takeoverLoop).toHaveBeenCalledWith("617");
      expect(onMutated).toHaveBeenCalled();
    });

    // Simulate detail refresh: loop is now human_takeover so recovery card
    // projection ends — result dialog must remain usable.
    rerender(
      <ToastProvider>
        <RecoveryCard
          loop={baseLoop({
            status: "human_takeover",
            displayStatus: "human_takeover",
            lastFailureKind: null,
            lastFailureReason: null,
          })}
          selector="617"
          onMutated={onMutated}
        />
      </ToastProvider>,
    );

    expect(screen.queryByText(/Manual intervention required/i)).toBeNull();
    const result = await screen.findByRole("dialog");
    expect(within(result).getByText(/Takeover result/i)).toBeTruthy();
    expect(within(result).getByText("/tmp/takeover-wt")).toBeTruthy();
    expect(within(result).getByText("codex resume abc")).toBeTruthy();
  });

  it("clears stale worktree UI when the selector changes", async () => {
    fetchLoopWorktree.mockImplementation(async (selector: string) => {
      if (selector === "617") {
        return {
          loopId: "loop_a",
          seq: 617,
          present: true,
          managed: true,
          dirty: true,
          clean: false,
          worktreePath: "/tmp/loop-a-dirty",
          reason: "dirty",
        };
      }
      return {
        loopId: "loop_b",
        seq: 618,
        present: true,
        managed: true,
        dirty: false,
        clean: true,
        worktreePath: "/tmp/loop-b-clean",
        reason: "already_clean",
      };
    });

    const { rerender } = render(
      <ToastProvider>
        <RecoveryCard loop={baseLoop({ seq: 617 })} selector="617" />
      </ToastProvider>,
    );

    await screen.findByText("/tmp/loop-a-dirty");
    expect(screen.getByRole("button", { name: "Discard & Retry" })).toBeTruthy();

    rerender(
      <ToastProvider>
        <RecoveryCard
          loop={baseLoop({
            id: "loop_2",
            seq: 618,
            lastFailureReason: "remote rejected",
          })}
          selector="618"
        />
      </ToastProvider>,
    );

    // Stale path from loop A must not remain visible while targeting B.
    expect(screen.queryByText("/tmp/loop-a-dirty")).toBeNull();
    expect(screen.queryByRole("button", { name: "Discard & Retry" })).toBeNull();
    await screen.findByText("/tmp/loop-b-clean");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});
