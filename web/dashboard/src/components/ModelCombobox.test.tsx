import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ModelCombobox } from "./ModelCombobox";

function jsonEnvelope(data: unknown) {
  return new Response(JSON.stringify({ ok: true, data }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

const MODELS = {
  vendor: "codex",
  models: [
    { id: "gpt-5", label: "GPT-5", source: "static" as const },
    { id: "gpt-5-mini", label: "GPT-5 mini", source: "probe" as const },
    { id: "o4", label: "O4", source: "probe" as const },
  ],
  sources: { static: true, probe: "ok" as const },
  probedAt: "2026-08-01T00:00:00Z",
};

describe("ModelCombobox", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const path = String(input);
        if (path.startsWith("/api/v1/agent/models")) {
          return jsonEnvelope(MODELS);
        }
        throw new Error(`unexpected: ${path}`);
      }),
    );
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    cleanup();
  });

  const commonProps = {
    ariaLabel: "agent.model",
    vendor: "codex",
    disabled: false,
    unset: false,
    allowInherit: true,
    onCommitValue: vi.fn(),
    onInherit: vi.fn(),
  };

  it("shows special rows and filters by id / label", async () => {
    const props = { ...commonProps, value: "" };
    render(<ModelCombobox {...props} />);

    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    fireEvent.focus(input);

    // Special rows present.
    expect(await screen.findByText("Inherit")).toBeTruthy();
    expect(screen.getByText("Vendor default")).toBeTruthy();

    // Suggestions rendered.
    await waitFor(() => {
      expect(screen.getByText("GPT-5")).toBeTruthy();
    });
    expect(screen.getByText("GPT-5 mini")).toBeTruthy();

    // Filter by id substring.
    fireEvent.change(input, { target: { value: "mini" } });
    await waitFor(() => {
      expect(screen.queryByText("GPT-5")).toBeNull();
    });
    expect(screen.getByText("GPT-5 mini")).toBeTruthy();
  });

  it("commits a picked suggestion via onCommitValue", async () => {
    const onCommit = vi.fn();
    const props = { ...commonProps, value: "", onCommitValue: onCommit };
    render(<ModelCombobox {...props} />);

    const input = screen.getByLabelText("agent.model");
    fireEvent.focus(input);

    const opt = await screen.findByText("GPT-5");
    fireEvent.mouseDown(opt);
    expect(onCommit).toHaveBeenCalledWith("gpt-5");
  });

  it("commits a custom typed id on Enter (keystrokes stay local)", () => {
    const onCommit = vi.fn();
    const props = { ...commonProps, value: "", onCommitValue: onCommit };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.change(input, { target: { value: "custom-id" } });
    // Typing is local until commit so Escape can cancel without a parent draft.
    expect(onCommit).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onCommit).toHaveBeenLastCalledWith("custom-id");
  });

  it("commits free-entry text on blur without Enter", () => {
    const onCommit = vi.fn();
    const props = { ...commonProps, value: "", onCommitValue: onCommit };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.change(input, { target: { value: "blur-commit" } });
    expect(onCommit).not.toHaveBeenCalled();
    fireEvent.blur(input);
    expect(onCommit).toHaveBeenLastCalledWith("blur-commit");
  });

  it("Inherit special row calls onInherit; Vendor default commits empty", async () => {
    const onCommit = vi.fn();
    const onInherit = vi.fn();
    const props = {
      ...commonProps,
      value: "gpt-5",
      onCommitValue: onCommit,
      onInherit,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.focus(input);

    fireEvent.mouseDown(await screen.findByText("Inherit"));
    expect(onInherit).toHaveBeenCalledTimes(1);
    expect(onCommit).not.toHaveBeenCalled();

    fireEvent.focus(input);
    fireEvent.mouseDown(await screen.findByText("Vendor default"));
    expect(onCommit).toHaveBeenLastCalledWith("");
  });

  it("does not fetch when vendor is null; prompts to select a vendor", async () => {
    const onCommit = vi.fn();
    const props = { ...commonProps, value: "", vendor: null, onCommitValue: onCommit };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.focus(input);
    expect(await screen.findByText(/Select a vendor first/i)).toBeTruthy();
    // Free entry still works (commit on Enter when no vendor).
    fireEvent.change(input, { target: { value: "custom" } });
    expect(onCommit).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onCommit).toHaveBeenLastCalledWith("custom");
  });

  it("keyboard navigation: ArrowDown starts at the current-state row", async () => {
    const onCommit = vi.fn();
    const props = { ...commonProps, value: "", onCommitValue: onCommit };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    fireEvent.focus(input);
    // Wait for options.
    await screen.findByText("GPT-5");
    // value="" → Vendor default is the current-state row. First ArrowDown
    // lands on it (rather than blindly on row 0); Enter commits "".
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onCommit).toHaveBeenLastCalledWith("");
  });

  it("untouched Enter with a typed custom id commits the typed value, not row zero", async () => {
    const onCommit = vi.fn();
    const onInherit = vi.fn();
    const props = {
      ...commonProps,
      value: "",
      onCommitValue: onCommit,
      onInherit,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.change(input, { target: { value: "my-model" } });
    // Keystrokes stay local; only Enter/pick/blur/Tab stage the draft.
    expect(onCommit).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Enter" });
    // Untouched Enter must not select Inherit (row 0) — it commits the typed
    // value (or falls back to no-op) so custom ids are not overwritten.
    expect(onInherit).not.toHaveBeenCalled();
    expect(onCommit).toHaveBeenLastCalledWith("my-model");
  });

  it("untouched Enter without a typed value keeps the parent value (no commit)", async () => {
    const onCommit = vi.fn();
    const onInherit = vi.fn();
    const props = {
      ...commonProps,
      value: "gpt-5",
      onCommitValue: onCommit,
      onInherit,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model");
    fireEvent.focus(input);
    await screen.findByText("GPT-5");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onCommit).not.toHaveBeenCalled();
    expect(onInherit).not.toHaveBeenCalled();
  });

  it("closes on Escape without staging, restoring parent value", async () => {
    const onCommit = vi.fn();
    const props = {
      ...commonProps,
      value: "gpt-5",
      onCommitValue: onCommit,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "typing…" } });
    expect(input.value).toBe("typing…");
    expect(onCommit).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Escape" });
    // Popup dismissed; input reverts to controlled parent value and Escape
    // must not leave a half-edited draft on the parent.
    expect(screen.queryByTestId("model-combobox-popup")).toBeNull();
    expect(input.value).toBe("gpt-5");
    expect(onCommit).not.toHaveBeenCalled();
  });

  it("shows the vendor-default placeholder when value is empty and not unset", () => {
    const props = { ...commonProps, value: "" };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.placeholder).toBe("Vendor default");
  });

  it("shows the inherit placeholder when unset", () => {
    const props = { ...commonProps, value: "", unset: true };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    expect(input.placeholder).toBe("Inherit");
    expect(input.disabled).toBe(true);
  });

  it("treats null value as Inherit (absent leaf) without locking the control", async () => {
    const onCommit = vi.fn();
    const onInherit = vi.fn();
    const props = {
      ...commonProps,
      value: null as string | null,
      onCommitValue: onCommit,
      onInherit,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    expect(input.placeholder).toBe("Inherit");
    expect(input.disabled).toBe(false);

    fireEvent.focus(input);
    await screen.findByText("GPT-5");
    // ArrowDown starts on Inherit (current state); Enter calls onInherit, not "".
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onInherit).toHaveBeenCalledTimes(1);
    expect(onCommit).not.toHaveBeenCalled();
  });

  it("keeps global absence distinct from explicit Vendor default suppress", async () => {
    const onCommit = vi.fn();
    const props = {
      ...commonProps,
      value: null as string | null,
      allowInherit: false,
      onInherit: undefined,
      onCommitValue: onCommit,
      placeholder: "Model id (empty = inherit CLI default)",
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    // Absent global model is unbound (params --model may still apply), not
    // the Vendor default suppress row.
    expect(input.placeholder).toBe("Model id (empty = inherit CLI default)");
    expect(input.placeholder).not.toBe("Vendor default");

    fireEvent.focus(input);
    await screen.findByText("GPT-5");
    // No current-state row: Vendor default is not aria-selected as current.
    const vendorDefault = screen.getByText("Vendor default").closest("[role='option']");
    expect(vendorDefault?.getAttribute("aria-selected")).toBe("false");

    // Untouched Enter must not stage explicit "" suppress.
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onCommit).not.toHaveBeenCalled();
  });

  it("explicit empty string still shows Vendor default when inherit is not allowed", () => {
    const props = {
      ...commonProps,
      value: "",
      allowInherit: false,
      onInherit: undefined,
    };
    render(<ModelCombobox {...props} />);
    const input = screen.getByLabelText("agent.model") as HTMLInputElement;
    expect(input.placeholder).toBe("Vendor default");
  });

  it("keeps cached suggestions when switching back to a fresh vendor while open", async () => {
    const codexModels = {
      vendor: "codex",
      models: [
        { id: "gpt-5", label: "GPT-5", source: "static" as const },
        { id: "o4", label: "O4", source: "probe" as const },
      ],
      sources: { static: true, probe: "ok" as const },
      probedAt: "2026-08-01T00:00:00Z",
    };
    const opencodeModels = {
      vendor: "opencode",
      models: [
        { id: "big-pickle", label: "Big Pickle", source: "static" as const },
      ],
      sources: { static: true, probe: "ok" as const },
      probedAt: "2026-08-01T00:00:00Z",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const path = String(input);
        if (path.includes("vendor=codex")) return jsonEnvelope(codexModels);
        if (path.includes("vendor=opencode")) return jsonEnvelope(opencodeModels);
        throw new Error(`unexpected: ${path}`);
      }),
    );

    const onCommit = vi.fn();
    const { rerender } = render(
      <ModelCombobox
        {...commonProps}
        value=""
        vendor="codex"
        onCommitValue={onCommit}
      />,
    );
    const input = screen.getByLabelText("agent.model");
    fireEvent.focus(input);
    await screen.findByText("GPT-5");

    // Visit opencode so both vendors are warm in the module cache.
    rerender(
      <ModelCombobox
        {...commonProps}
        value=""
        vendor="opencode"
        onCommitValue={onCommit}
      />,
    );
    await screen.findByText("Big Pickle");
    expect(screen.queryByText("GPT-5")).toBeNull();

    // Switch back to codex while still open. Fresh cache must be restored and
    // must not be wiped by a later vendor-clear effect (only specials left).
    rerender(
      <ModelCombobox
        {...commonProps}
        value=""
        vendor="codex"
        onCommitValue={onCommit}
      />,
    );
    await waitFor(() => {
      expect(screen.getByText("GPT-5")).toBeTruthy();
    });
    expect(screen.getByText("O4")).toBeTruthy();
    expect(screen.queryByText("Big Pickle")).toBeNull();
  });
});
