import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PullRequestLink } from "@/components/PullRequestLink";

afterEach(() => {
  cleanup();
});

describe("PullRequestLink", () => {
  it("stops click and Enter/Space keydown from bubbling to row handlers", () => {
    const onRowClick = vi.fn();
    const onRowKeyDown = vi.fn();

    render(
      <div
        data-testid="row"
        tabIndex={0}
        onClick={onRowClick}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onRowKeyDown(e);
          }
        }}
      >
        <PullRequestLink href="https://github.com/acme/looper/pull/1">
          #1
        </PullRequestLink>
      </div>,
    );

    const link = screen.getByRole("link", { name: /#1/ });
    fireEvent.click(link);
    expect(onRowClick).not.toHaveBeenCalled();

    fireEvent.keyDown(link, { key: "Enter" });
    fireEvent.keyDown(link, { key: " " });
    expect(onRowKeyDown).not.toHaveBeenCalled();
  });
});
