import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  classifyRetryWorktree,
  isWorktreeRouteUnavailable,
  LoopActionBar,
} from "@/components/LoopActionBar";
import { ApiError } from "@/lib/api";
import { ToastProvider } from "@/lib/toast";

const fetchLoopWorktree = vi.fn();
const retryLoop = vi.fn();
const pauseLoop = vi.fn();
const startLoop = vi.fn();
const stopActiveRun = vi.fn();
const takeoverLoop = vi.fn();
const handbackLoop = vi.fn();

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    fetchLoopWorktree: (...args: unknown[]) => fetchLoopWorktree(...args),
    retryLoop: (...args: unknown[]) => retryLoop(...args),
    pauseLoop: (...args: unknown[]) => pauseLoop(...args),
    startLoop: (...args: unknown[]) => startLoop(...args),
    stopActiveRun: (...args: unknown[]) => stopActiveRun(...args),
    takeoverLoop: (...args: unknown[]) => takeoverLoop(...args),
    handbackLoop: (...args: unknown[]) => handbackLoop(...args),
  };
});

function renderBar(props?: Partial<React.ComponentProps<typeof LoopActionBar>>) {
  const onMutated = vi.fn().mockResolvedValue(undefined);
  render(
    <ToastProvider>
      <LoopActionBar
        selector="3491"
        status="paused"
        mode="full"
        onMutated={onMutated}
        {...props}
      />
    </ToastProvider>,
  );
  return { onMutated };
}

describe("classifyRetryWorktree", () => {
  it("offers discard only for present managed dirty trees", () => {
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        dirty: true,
      }),
    ).toBe("offer-discard");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: false,
        dirty: true,
      }),
    ).toBe("inspect-only");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        dirty: false,
      }),
    ).toBe("ok");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: false,
        managed: true,
        dirty: true,
      }),
    ).toBe("ok");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        // dirty unknown → fail closed (shared with recovery card)
      }),
    ).toBe("inspect-only");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        reason: "unusable_path",
        supportsClearUnusablePath: true,
      }),
    ).toBe("offer-clear");
    expect(
      classifyRetryWorktree({
        loopId: "l",
        seq: 1,
        present: true,
        managed: true,
        reason: "unusable_path",
        // older daemon omits capability → do not offer clear
      }),
    ).toBe("inspect-only");
  });
});

describe("isWorktreeRouteUnavailable", () => {
  it("detects older-daemon route missing", () => {
    expect(
      isWorktreeRouteUnavailable(
        new ApiError("Unknown route", {
          status: 404,
          code: "ROUTE_NOT_FOUND",
        }),
      ),
    ).toBe(true);
    expect(
      isWorktreeRouteUnavailable(
        new ApiError("boom", { status: 500, code: "INTERNAL_ERROR" }),
      ),
    ).toBe(false);
  });
});

describe("LoopActionBar retry dirty UX", () => {
  beforeEach(() => {
    fetchLoopWorktree.mockReset();
    retryLoop.mockReset();
    pauseLoop.mockReset();
    startLoop.mockReset();
    stopActiveRun.mockReset();
    takeoverLoop.mockReset();
    handbackLoop.mockReset();
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

  it("retries immediately when worktree is clean", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: false,
      clean: true,
      worktreePath: "/tmp/wt",
    });
    const { onMutated } = renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: false,
      });
    });
    expect(onMutated).toHaveBeenCalled();
    expect(screen.queryByText(/Dirty worktree/i)).toBeNull();
  });

  it("confirms discard for managed dirty worktree and posts discard=true", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/dirty-wt",
      branch: "feat/x",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await screen.findByText(/Dirty worktree — discard and retry/i);
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Discard & retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: true,
      });
    });
  });

  it("confirms clear for managed unusable_path and posts clearUnusableWorktreePath", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      reason: "unusable_path",
      worktreePath: "/tmp/hollow-wt",
      supportsClearUnusablePath: true,
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await screen.findByText(/Unusable worktree path — clear and retry/i);
    expect(retryLoop).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Clear path & retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: false,
        clearUnusableWorktreePath: true,
        expectedWorktreePath: "/tmp/hollow-wt",
      });
    });
  });

  it("inspect-first cancels discard and shows jump guidance", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: true,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/dirty-wt",
      branch: "feat/x",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText(/Dirty worktree — discard and retry/i);

    fireEvent.click(screen.getByRole("button", { name: "Inspect first" }));

    await screen.findByText(/Inspect dirty worktree/i);
    expect(screen.getByText("looper jump 3491")).toBeTruthy();
    expect(screen.getByText("/tmp/dirty-wt")).toBeTruthy();
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("does not offer discard for unmanaged dirty worktree", async () => {
    fetchLoopWorktree.mockResolvedValue({
      loopId: "loop_1",
      seq: 3491,
      present: true,
      managed: false,
      dirty: true,
      clean: false,
      worktreePath: "/tmp/primary-repo",
      reason: "unmanaged",
    });
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await screen.findByText(/Unmanaged dirty worktree/i);
    expect(screen.queryByText("Discard & retry")).toBeNull();
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("falls back to plain retry when /worktree route is missing", async () => {
    fetchLoopWorktree.mockRejectedValue(
      new ApiError("Unknown route", {
        status: 404,
        code: "ROUTE_NOT_FOUND",
      }),
    );
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(retryLoop).toHaveBeenCalledWith("3491", {
        discardWorktreeChanges: false,
      });
    });
  });

  it("surfaces non-404 preflight failures without retrying", async () => {
    fetchLoopWorktree.mockRejectedValue(
      new ApiError("git status failed", {
        status: 500,
        code: "INTERNAL_ERROR",
      }),
    );
    renderBar();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(screen.getAllByText("git status failed").length).toBeGreaterThan(0);
    });
    expect(retryLoop).not.toHaveBeenCalled();
  });

  it("disables Unpause when displayStatus is manual_intervention", () => {
    renderBar({ displayStatus: "manual_intervention", status: "paused" });
    const unpause = screen.getByRole("button", { name: "Unpause" });
    expect(unpause).toHaveProperty("disabled", true);
    fireEvent.click(unpause);
    expect(startLoop).not.toHaveBeenCalled();
  });

  it("enables Unpause when paused without manual_intervention projection", () => {
    renderBar({ status: "paused" });
    const unpause = screen.getByRole("button", { name: "Unpause" });
    expect(unpause).toHaveProperty("disabled", false);
  });
});
