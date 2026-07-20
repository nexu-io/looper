import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "@/App";
import { navItems } from "@/components/layout/Shell";
import type { ConfigData } from "@/lib/api";
import { ToastProvider } from "@/lib/toast";
import { ConfigPage } from "./Config";

function configFixture(overrides: Partial<ConfigData> = {}): ConfigData {
  return {
    scheduler: {
      maxConcurrentRuns: 3,
      retryMaxAttempts: 4,
      retryBaseDelayMs: 5000,
      slowLaneWarnThresholdMs: 5000,
    },
    agent: {
      vendor: "codex",
      model: "gpt-5",
      nativeResume: { enabled: true },
      timeouts: { plannerIdleTimeoutSeconds: 300 },
      envKeys: ["OPENAI_API_KEY"],
    },
    defaults: {
      allowAutoPush: false,
    },
    metadata: {
      configPath: "/tmp/config.toml",
      format: "toml",
      filePresent: true,
      revision: "sha256:test",
      lastAppliedAt: "2026-07-16T01:00:00Z",
      fields: {
        "scheduler.maxConcurrentRuns": {
          source: "env",
          editable: false,
          applyMode: "hot",
        },
        "scheduler.slowLaneWarnThresholdMs": {
          source: "default",
          editable: true,
          applyMode: "hot",
        },
        "agent.vendor": { source: "config-file", editable: true, applyMode: "hot" },
        "agent.model": { source: "config-file", editable: true, applyMode: "hot" },
        "agent.nativeResume.enabled": {
          source: "default",
          editable: false,
          applyMode: "restart",
        },
        "agent.timeouts.plannerIdleTimeoutSeconds": {
          source: "default",
          editable: true,
          applyMode: "hot",
        },
        "agent.env": { source: "config-file", editable: true, applyMode: "hot" },
        "agent.env.OPENAI_API_KEY": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "defaults.allowAutoPush": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
      },
    },
    ...overrides,
  };
}

function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400
        ? { ok: false, error: data }
        : { ok: true, data, error: null },
    ),
    {
      status,
      headers: { "Content-Type": "application/json" },
    },
  );
}

function renderPage() {
  return render(
    <ToastProvider>
      <ConfigPage />
    </ToastProvider>,
  );
}

/** Header + sticky footer both expose Save while dirty. */
function saveButtons(): HTMLButtonElement[] {
  return screen.getAllByRole("button", {
    name: "Save changes",
  }) as HTMLButtonElement[];
}

function clickSaveChanges() {
  fireEvent.click(saveButtons()[0]);
}

function expectSaveDisabled(disabled: boolean) {
  for (const button of saveButtons()) {
    expect(button.disabled).toBe(disabled);
  }
}

function discardButtons(): HTMLButtonElement[] {
  return screen.getAllByRole("button", {
    name: "Discard",
  }) as HTMLButtonElement[];
}

function clickDiscard() {
  fireEvent.click(discardButtons()[0]);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

describe("ConfigPage", () => {
  it("renders the /config route and its navigation item", async () => {
    window.history.replaceState({}, "", "/dashboard/config");
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/api/v1/healthz") return response({ healthy: true });
      if (path === "/api/v1/runs/active") return response({ items: [] });
      if (path === "/api/v1/projects") return response({ items: [] });
      if (path === "/api/v1/config") return response(configFixture());
      throw new Error(`unexpected request: ${path}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<App />);

    expect(await screen.findByRole("heading", { name: "Configuration" })).toBeTruthy();
    const configLink = screen.getByRole("link", { name: "Config" });
    expect(configLink.getAttribute("href")).toBe("/dashboard/config");
    expect(navItems).toContainEqual({ to: "/config", label: "Config" });
  });

  it("locks env/CLI winners and uses the authoritative PATCH snapshot", async () => {
    const initial = configFixture();
    const applied = configFixture({
      scheduler: {
        ...(initial.scheduler ?? {}),
        slowLaneWarnThresholdMs: 8,
      },
    });
    let getCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path !== "/api/v1/config") throw new Error(`unexpected request: ${path}`);
      if (init?.method === "PATCH") return response(applied);
      getCount += 1;
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const locked = (await screen.findByLabelText(
      "scheduler.maxConcurrentRuns",
    )) as HTMLInputElement;
    expect(locked.disabled).toBe(true);
    expect(screen.getByText(/ENV is the active authority/i)).toBeTruthy();
    expect(screen.queryByLabelText("agent.nativeResume.enabled")).toBeNull();

    const retry = screen.getByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    ) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "8" } });
    clickSaveChanges();

    await waitFor(() => {
      expect(
        (screen.getByLabelText("scheduler.slowLaneWarnThresholdMs") as HTMLInputElement)
          .value,
      ).toBe("8");
    });
    expect(getCount).toBe(1);
    const patchCall = fetchMock.mock.calls.find(
      ([, init]) => init?.method === "PATCH",
    );
    expect(patchCall).toBeTruthy();
    expect(JSON.parse(String(patchCall?.[1]?.body))).toEqual({
      revision: "sha256:test",
      set: { "scheduler.slowLaneWarnThresholdMs": 8 },
      unset: [],
    });
  });

  it("does not let an older in-flight refresh overwrite a saved snapshot", async () => {
    const initial = configFixture();
    const applied = configFixture({
      scheduler: {
        ...(initial.scheduler ?? {}),
        slowLaneWarnThresholdMs: 8,
      },
      metadata: {
        ...initial.metadata,
        revision: "sha256:applied",
      },
    });
    let getCount = 0;
    let resolveRefresh: ((value: Response) => void) | undefined;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const path = String(input);
        if (path !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${path}`));
        }
        if (init?.method === "PATCH") return Promise.resolve(response(applied));
        getCount += 1;
        if (getCount === 1) return Promise.resolve(response(initial));
        if (getCount === 2) {
          return Promise.resolve(response({ message: "refresh failed" }, 500));
        }
        return new Promise((resolve) => {
          resolveRefresh = resolve;
        });
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByText("refresh failed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(getCount).toBe(3));

    fireEvent.change(retry, { target: { value: "8" } });
    clickSaveChanges();
    await waitFor(() => expect(retry.value).toBe("8"));
    expect(screen.queryByText("refresh failed")).toBeNull();
    expect(resolveRefresh).toBeTypeOf("function");

    await act(async () => {
      resolveRefresh?.(response(initial));
      await Promise.resolve();
    });

    expect(retry.value).toBe("8");
  });

  it("aborts an in-flight refresh when the user starts editing", async () => {
    const initial = configFixture();
    const refreshed = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 8 },
      metadata: { ...initial.metadata, revision: "sha256:external" },
    });
    const applied = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 6 },
      metadata: { ...initial.metadata, revision: "sha256:applied" },
    });
    let getCount = 0;
    let resolveRefresh: ((value: Response) => void) | undefined;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") return Promise.resolve(response(applied));
        getCount += 1;
        if (getCount === 1) return Promise.resolve(response(initial));
        return new Promise((resolve) => {
          resolveRefresh = resolve;
        });
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(getCount).toBe(2));
    fireEvent.change(retry, { target: { value: "6" } });
    await act(async () => {
      resolveRefresh?.(response(refreshed));
      await Promise.resolve();
    });
    expect(retry.value).toBe("6");

    clickSaveChanges();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    expect(JSON.parse(String(patchCall?.[1]?.body))).toMatchObject({
      revision: "sha256:test",
      set: { "scheduler.slowLaneWarnThresholdMs": 6 },
    });
  });

  it("rebases retained drafts explicitly after a revision conflict", async () => {
    const initial = configFixture();
    const refreshed = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 8 },
      metadata: { ...initial.metadata, revision: "sha256:external" },
    });
    const applied = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 6 },
      metadata: { ...initial.metadata, revision: "sha256:applied" },
    });
    let getCount = 0;
    let patchCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          patchCount += 1;
          if (patchCount === 1) {
            return Promise.resolve(
              response(
                { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
                409,
              ),
            );
          }
          return Promise.resolve(response(applied));
        }
        getCount += 1;
        return Promise.resolve(response(getCount === 1 ? initial : refreshed));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const { container } = renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "6" } });
    clickSaveChanges();
    const rebase = await screen.findByRole("button", {
      name: "Reload latest and keep edits",
    });
    expect(retry.value).toBe("6");
    expect(retry.disabled).toBe(true);
    expectSaveDisabled(true);
    fireEvent.click(rebase);

    await waitFor(() => {
      const field = container.querySelector(
        '[data-config-path="scheduler.slowLaneWarnThresholdMs"]',
      );
      expect(field?.textContent).toMatch(/Unsaved draft/i);
      expect(field?.textContent).toContain("8");
    });
    expect(retry.value).toBe("6");
    clickSaveChanges();
    await waitFor(() => expect(patchCount).toBe(2));
    const patchCalls = fetchMock.mock.calls.filter(
      ([, init]) => init?.method === "PATCH",
    );
    expect(JSON.parse(String(patchCalls[1]?.[1]?.body))).toMatchObject({
      revision: "sha256:external",
      set: { "scheduler.slowLaneWarnThresholdMs": 6 },
    });
  });

  it("keeps a conflict open while the changed file is still rejected", async () => {
    const initial = configFixture();
    const rejected = configFixture({
      metadata: {
        ...initial.metadata,
        revision: initial.metadata.revision,
        lastError: "configuration reload rejected",
        rejectedPaths: ["server.port"],
      },
    });
    let getCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          return Promise.resolve(
            response(
              { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
              409,
            ),
          );
        }
        getCount += 1;
        return Promise.resolve(response(getCount === 1 ? initial : rejected));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.change(
      await screen.findByLabelText("scheduler.slowLaneWarnThresholdMs"),
      { target: { value: "6" } },
    );
    clickSaveChanges();
    fireEvent.click(
      await screen.findByRole("button", {
        name: "Reload latest and keep edits",
      }),
    );

    expect(
      await screen.findByText(/changed config file is still rejected/i),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Reload latest and keep edits" }),
    ).toBeTruthy();
    expect(
      (screen.getByLabelText("scheduler.slowLaneWarnThresholdMs") as HTMLInputElement)
        .value,
    ).toBe("6");
  });

  it("unlocks a same-revision conflict when the daemon has no reload error", async () => {
    // PATCH can 409 without changing the accepted content hash (identity race,
    // cancel). Rebase must not stay locked forever when revision is unchanged
    // but lastError is empty — OCC will re-check on the next save.
    const initial = configFixture();
    const sameRevision = configFixture({
      metadata: {
        ...initial.metadata,
        revision: initial.metadata.revision,
        lastError: null,
      },
    });
    const applied = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 6 },
      metadata: { ...initial.metadata, revision: "sha256:applied" },
    });
    let getCount = 0;
    let patchCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          patchCount += 1;
          if (patchCount === 1) {
            return Promise.resolve(
              response(
                { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
                409,
              ),
            );
          }
          return Promise.resolve(response(applied));
        }
        getCount += 1;
        return Promise.resolve(response(getCount === 1 ? initial : sameRevision));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.change(
      await screen.findByLabelText("scheduler.slowLaneWarnThresholdMs"),
      { target: { value: "6" } },
    );
    clickSaveChanges();
    fireEvent.click(
      await screen.findByRole("button", {
        name: "Reload latest and keep edits",
      }),
    );

    await waitFor(() => {
      expectSaveDisabled(false);
    });
    expect(
      screen.queryByText(/changed config file is still rejected/i),
    ).toBeNull();
    clickSaveChanges();
    await waitFor(() => expect(patchCount).toBe(2));
  });

  it("prunes a retained draft that already matches the rebased snapshot", async () => {
    const initial = configFixture();
    const matching = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 8 },
      metadata: { ...initial.metadata, revision: "sha256:external" },
    });
    const later = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 9 },
      metadata: { ...initial.metadata, revision: "sha256:later" },
    });
    let getCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          return Promise.resolve(
            response(
              { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
              409,
            ),
          );
        }
        getCount += 1;
        return Promise.resolve(
          response(getCount === 1 ? initial : getCount === 2 ? matching : later),
        );
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    const { container } = renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "8" } });
    clickSaveChanges();
    fireEvent.click(
      await screen.findByRole("button", {
        name: "Reload latest and keep edits",
      }),
    );

    expect(await screen.findByText(/change now matches/i)).toBeTruthy();
    expect(retry.value).toBe("8");
    expectSaveDisabled(true);
    const field = container.querySelector(
      '[data-config-path="scheduler.slowLaneWarnThresholdMs"]',
    );
    expect(field?.textContent).not.toMatch(/Unsaved draft/i);

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(retry.value).toBe("9"));
  });

  it("clears write-only operations when rebasing after a conflict", async () => {
    const initial = configFixture();
    const refreshed = configFixture({
      metadata: { ...initial.metadata, revision: "sha256:external" },
    });
    let getCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          return Promise.resolve(
            response(
              { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
              409,
            ),
          );
        }
        getCount += 1;
        return Promise.resolve(response(getCount === 1 ? initial : refreshed));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.change(await screen.findByLabelText("Environment variable name"), {
      target: { value: "NEW_SECRET" },
    });
    fireEvent.change(screen.getByLabelText("Environment variable secret"), {
      target: { value: "secret-value" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Stage secret" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove OPENAI_API_KEY" }));
    clickSaveChanges();
    fireEvent.click(
      await screen.findByRole("button", {
        name: "Reload latest and keep edits",
      }),
    );

    expect(
      await screen.findByText(/write-only agent environment changes were cleared/i),
    ).toBeTruthy();
    expect(screen.queryByText("NEW_SECRET")).toBeNull();
    expect(
      screen.getByRole("button", { name: "Remove OPENAI_API_KEY" }),
    ).toBeTruthy();
    expectSaveDisabled(true);
  });

  it("tracks and discards typed but unstaged environment input", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => response(configFixture())),
    );
    renderPage();

    const key = (await screen.findByLabelText(
      "Environment variable name",
    )) as HTMLInputElement;
    const secret = screen.getByLabelText(
      "Environment variable secret",
    ) as HTMLInputElement;
    const retry = screen.getByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    ) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "6" } });
    fireEvent.change(key, { target: { value: "UNSTAGED" } });
    fireEvent.change(secret, { target: { value: "not-saved" } });

    expect(discardButtons()[0].disabled).toBe(false);
    expect(
      (screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expectSaveDisabled(true);

    clickDiscard();
    expect(
      (screen.getByLabelText("Environment variable name") as HTMLInputElement)
        .value,
    ).toBe("");
    expect(
      (screen.getByLabelText("Environment variable secret") as HTMLInputElement)
        .value,
    ).toBe("");
    expect(
      (screen.getByLabelText("scheduler.slowLaneWarnThresholdMs") as HTMLInputElement)
        .value,
    ).toBe("5000");
  });

  it("aborts an in-flight refresh synchronously when environment input is typed", async () => {
    const initial = configFixture();
    const refreshed = configFixture({
      metadata: { ...initial.metadata, revision: "sha256:refreshed" },
    });
    let getCount = 0;
    let resolveRefresh: ((value: Response) => void) | undefined;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        getCount += 1;
        if (getCount === 1) return Promise.resolve(response(initial));
        return new Promise((resolve) => {
          resolveRefresh = resolve;
        });
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const key = (await screen.findByLabelText(
      "Environment variable name",
    )) as HTMLInputElement;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(resolveRefresh).toBeTypeOf("function"));
    fireEvent.change(key, { target: { value: "KEEP_ME" } });

    await act(async () => {
      resolveRefresh?.(response(refreshed));
      await Promise.resolve();
    });
    expect(key.value).toBe("KEEP_ME");
  });

  it("hides a failed-refresh retry while local input is pending", async () => {
    let getCount = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        getCount += 1;
        if (getCount === 1) return response(configFixture());
        return response({ message: "refresh failed" }, 500);
      }),
    );
    renderPage();

    await screen.findByLabelText("Environment variable name");
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByText("refresh failed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Environment variable name"), {
      target: { value: "KEEP_LOCAL" },
    });
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(
      (screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("locks all editors while a PATCH is in flight", async () => {
    const initial = configFixture();
    const applied = configFixture({
      scheduler: { ...(initial.scheduler ?? {}), slowLaneWarnThresholdMs: 8 },
      metadata: { ...initial.metadata, revision: "sha256:applied" },
    });
    let resolvePatch: ((value: Response) => void) | undefined;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          return new Promise((resolve) => {
            resolvePatch = resolve;
          });
        }
        return Promise.resolve(response(initial));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "8" } });
    clickSaveChanges();
    await waitFor(() => expect(resolvePatch).toBeTypeOf("function"));

    expect(retry.disabled).toBe(true);
    expect(
      (screen.getByLabelText("Environment variable name") as HTMLInputElement)
        .disabled,
    ).toBe(true);

    await act(async () => {
      resolvePatch?.(response(applied));
      await Promise.resolve();
    });
    await waitFor(() =>
      expect(
        (screen.getByLabelText(
          "scheduler.slowLaneWarnThresholdMs",
        ) as HTMLInputElement).value,
      ).toBe("8"),
    );
  });

  it("shows client and backend field validation errors", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path !== "/api/v1/config") throw new Error(`unexpected request: ${path}`);
      if (init?.method === "PATCH") {
        return response(
          {
            code: "invalid_argument",
            message: "configuration is invalid",
            details: {
              issues: [
                {
                  path: "scheduler.slowLaneWarnThresholdMs",
                  message: "must be -1 or greater than zero",
                },
              ],
            },
          },
          400,
        );
      }
      return response(configFixture());
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const retry = (await screen.findByLabelText(
      "scheduler.slowLaneWarnThresholdMs",
    )) as HTMLInputElement;
    fireEvent.change(retry, { target: { value: "3.5" } });
    clickSaveChanges();
    expect(await screen.findByText(/enter a whole number/i)).toBeTruthy();
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
      false,
    );

    fireEvent.change(retry, { target: { value: "0" } });
    clickSaveChanges();
    expect(
      await screen.findByText(/must be -1 or greater than zero/i),
    ).toBeTruthy();
  });

  it("keeps agent env values write-only while supporting set and remove", async () => {
    const base = configFixture();
    const secretSafeFixture = configFixture({
      agent: {
        ...base.agent,
        envKeys: ["OPENAI_API_KEY", "LOCKED_SECRET"],
        // Defensive regression fixture: even if an older daemon returned this
        // deprecated shape, the curated UI must never recursively render it.
        env: { OPENAI_API_KEY: "existing-secret-value" },
      } as ConfigData["agent"],
      metadata: {
        ...base.metadata,
        fields: {
          ...base.metadata.fields,
          "agent.env.LOCKED_SECRET": {
            source: "env",
            editable: false,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path !== "/api/v1/config") throw new Error(`unexpected request: ${path}`);
      return response(secretSafeFixture);
    });
    vi.stubGlobal("fetch", fetchMock);
    const { container } = renderPage();

    expect(await screen.findByText("OPENAI_API_KEY")).toBeTruthy();
    expect(screen.getByText(/values are write-only/i)).toBeTruthy();
    expect(container.textContent).not.toContain("existing-secret-value");
    expect(container.querySelector('[name="agent.env"]')).toBeNull();

    const lockedRemove = screen.getByRole("button", {
      name: "Remove LOCKED_SECRET",
    }) as HTMLButtonElement;
    expect(lockedRemove.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Environment variable name"), {
      target: { value: "LOCKED_SECRET" },
    });
    fireEvent.change(screen.getByLabelText("Environment variable secret"), {
      target: { value: "must-not-stage" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Stage secret" }));
    expect(await screen.findByText(/higher-precedence authority/i)).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Environment variable name"), {
      target: { value: "NEW_SECRET" },
    });
    const secretInput = screen.getByLabelText(
      "Environment variable secret",
    ) as HTMLInputElement;
    fireEvent.change(secretInput, { target: { value: "super-secret-value" } });
    fireEvent.click(screen.getByRole("button", { name: "Stage secret" }));

    expect(secretInput.value).toBe("");
    expect(container.textContent).not.toContain("super-secret-value");
    expect(screen.getByText("NEW_SECRET")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove OPENAI_API_KEY" }));
    clickSaveChanges();

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(
      ([, init]) => init?.method === "PATCH",
    );
    expect(JSON.parse(String(patchCall?.[1]?.body))).toEqual({
      revision: "sha256:test",
      set: { "agent.env.NEW_SECRET": "super-secret-value" },
      unset: ["agent.env.OPENAI_API_KEY"],
    });
  });

  it("confirms high-impact enables before sending PATCH", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path !== "/api/v1/config") throw new Error(`unexpected request: ${path}`);
      return response(configFixture());
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const autoPush = (await screen.findByLabelText(
      "defaults.allowAutoPush",
    )) as HTMLInputElement;
    fireEvent.click(autoPush);
    clickSaveChanges();

    expect(
      await screen.findByRole("dialog", {
        name: "Confirm high-impact configuration",
      }),
    ).toBeTruthy();
    expect(
      discardButtons()[0].disabled,
    ).toBe(true);
    expect(
      (screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expectSaveDisabled(true);
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
      false,
    );
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
  });

  it("promotes unsetting both profile leaves to whole-profile removal", async () => {
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    // First leaf unset stays leaf-level while the other identity remains.
    fireEvent.click(
      screen.getByRole("button", { name: "Unset agent.profiles.fast.model" }),
    );
    // Last remaining leaf promotes to whole-profile removal (avoids empty {}).
    fireEvent.click(
      screen.getByRole("button", { name: "Unset agent.profiles.fast.vendor" }),
    );
    // Profile shows as pending removal, not dual leaf unsets.
    expect(screen.getByText("undo remove")).toBeTruthy();
    clickSaveChanges();

    // Unreferenced whole-profile remove is not high-impact; PATCH immediately.
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set).toEqual({});
    expect(body.unset).toEqual(["agent.profiles.fast"]);
  });

  it("does not retain omitted profile leaves when re-adding after remove", async () => {
    // remove+recreate with only vendor must unset the old model rather than
    // restoring the whole profile and silently keeping the previous model leaf.
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove profile fast" }));
    expect(screen.getByText("undo remove")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("New profile id"), {
      target: { value: "fast" },
    });
    fireEvent.change(screen.getByLabelText("New profile vendor"), {
      target: { value: "opencode" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add profile" }));
    clickSaveChanges();

    // Vendor change is high-impact (resolved-vendor companion guard).
    fireEvent.click(
      await screen.findByRole("button", { name: "Apply changes" }),
    );

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set).toEqual({ "agent.profiles.fast.vendor": "opencode" });
    expect(body.unset).toEqual(["agent.profiles.fast.model"]);
    expect(body.unset).not.toContain("agent.profiles.fast");
  });

  it("unsets empty model when re-adding a profile after remove with only vendor", async () => {
    // Empty model is a meaningful suppress binding; remove+recreate with only
    // vendor must unset it so the new profile does not keep model:"".
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove profile fast" }));
    expect(screen.getByText("undo remove")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("New profile id"), {
      target: { value: "fast" },
    });
    fireEvent.change(screen.getByLabelText("New profile vendor"), {
      target: { value: "opencode" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add profile" }));
    clickSaveChanges();

    fireEvent.click(
      await screen.findByRole("button", { name: "Apply changes" }),
    );

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set).toEqual({ "agent.profiles.fast.vendor": "opencode" });
    expect(body.unset).toEqual(["agent.profiles.fast.model"]);
    expect(body.unset).not.toContain("agent.profiles.fast");
  });

  it("unsets a single profile model leaf without removing the profile", async () => {
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: "Unset agent.profiles.fast.model" }),
    );
    clickSaveChanges();

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set).toEqual({});
    expect(body.unset).toEqual(["agent.profiles.fast.model"]);
  });

  it("keeps staged profile leaf edits when rebasing after a conflict", async () => {
    const baseMeta = {
      ...configFixture().metadata.fields,
      "agent.profiles": {
        source: "config-file" as const,
        editable: true,
        applyMode: "hot" as const,
      },
    };
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: baseMeta,
      },
    });
    const refreshed = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        revision: "sha256:external",
        // Leaf metadata intentionally omitted — only the map entry exists.
        fields: baseMeta,
      },
    });
    const applied = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        revision: "sha256:applied",
        fields: baseMeta,
      },
    });
    let getCount = 0;
    let patchCount = 0;
    const fetchMock = vi.fn(
      (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        if (String(input) !== "/api/v1/config") {
          return Promise.reject(new Error(`unexpected request: ${String(input)}`));
        }
        if (init?.method === "PATCH") {
          patchCount += 1;
          if (patchCount === 1) {
            return Promise.resolve(
              response(
                { code: "CONFIG_CONFLICT", message: "configuration changed on disk" },
                409,
              ),
            );
          }
          return Promise.resolve(response(applied));
        }
        getCount += 1;
        return Promise.resolve(response(getCount === 1 ? initial : refreshed));
      },
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.change(await screen.findByLabelText("agent.profiles.fast.model"), {
      target: { value: "gpt-5" },
    });
    clickSaveChanges();
    fireEvent.click(
      await screen.findByRole("button", {
        name: "Reload latest and keep edits",
      }),
    );

    await waitFor(() => {
      expect(
        (screen.getByLabelText("agent.profiles.fast.model") as HTMLInputElement).value,
      ).toBe("gpt-5");
    });
    clickSaveChanges();
    await waitFor(() => expect(patchCount).toBe(2));
    const patchCalls = fetchMock.mock.calls.filter(
      ([, init]) => init?.method === "PATCH",
    );
    expect(JSON.parse(String(patchCalls[1]?.[1]?.body))).toMatchObject({
      revision: "sha256:external",
      set: { "agent.profiles.fast.model": "gpt-5" },
    });
  });

  it("confirms high-impact role agent vendor changes before PATCH", async () => {
    const initial = configFixture({
      roles: {
        worker: { agent: { vendor: "codex" } },
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "roles.worker.agent.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.change(await screen.findByLabelText("roles.worker.agent.vendor"), {
      target: { value: "opencode" },
    });
    clickSaveChanges();

    expect(
      await screen.findByRole("dialog", {
        name: "Confirm high-impact configuration",
      }),
    ).toBeTruthy();
    expect(screen.getByText(/worker agent vendor → opencode/i)).toBeTruthy();
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
      false,
    );
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
  });

  it("edits agent profiles and role agent bindings without a params editor", async () => {
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      roles: {
        worker: {
          agent: { profile: "fast", model: "haiku" },
        },
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "roles.worker.agent.profile": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "roles.worker.agent.vendor": {
            source: "default",
            editable: true,
            applyMode: "hot",
          },
          "roles.worker.agent.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    expect(screen.queryByLabelText(/params/i)).toBeNull();
    expect(screen.queryByText(/params map/i)).toBeNull();

    fireEvent.change(screen.getByLabelText("agent.profiles.fast.model"), {
      target: { value: "gpt-5" },
    });
    fireEvent.change(screen.getByLabelText("New profile id"), {
      target: { value: "cheap" },
    });
    fireEvent.change(screen.getByLabelText("New profile vendor"), {
      target: { value: "opencode" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add profile" }));
    fireEvent.change(screen.getByLabelText("roles.worker.agent.profile"), {
      target: { value: "cheap" },
    });
    clickSaveChanges();

    // Profile switch is high-impact (can change resolved vendor).
    fireEvent.click(
      await screen.findByRole("button", { name: "Apply changes" }),
    );

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set["agent.profiles.fast.model"]).toBe("gpt-5");
    expect(body.set["agent.profiles.cheap.vendor"]).toBe("opencode");
    expect(body.set["roles.worker.agent.profile"]).toBe("cheap");
    expect(JSON.stringify(body)).not.toMatch(/params/i);
  });

  it("retains cleared role profile draft so Save sends unset", async () => {
    // Clearing the text control stages only an unset (no set). onDraft must
    // keep the empty draft; otherwise the field snaps back and Save is a no-op.
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      roles: {
        worker: {
          agent: { profile: "fast", vendor: "claude-code", model: "haiku" },
        },
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "roles.worker.agent.profile": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "roles.worker.agent.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "roles.worker.agent.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const profileInput = (await screen.findByLabelText(
      "roles.worker.agent.profile",
    )) as HTMLInputElement;
    expect(profileInput.value).toBe("fast");
    fireEvent.change(profileInput, { target: { value: "" } });
    // Empty draft must stick (not snap back to published "fast").
    expect(profileInput.value).toBe("");
    expect(screen.getAllByText(/1\s*unsaved/i).length).toBeGreaterThan(0);

    clickSaveChanges();
    // Unsetting a role profile is high-impact.
    fireEvent.click(
      await screen.findByRole("button", { name: "Apply changes" }),
    );

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set).toEqual({});
    expect(body.unset).toEqual(["roles.worker.agent.profile"]);
  });

  it("keeps profile controls editable when map container metadata is restart-bound", async () => {
    // Mirrors real daemon metadata: agent.profiles is a non-editable map
    // container while entry/leaf paths remain hot-editable.
    const initial = configFixture({
      agent: {
        vendor: "codex",
        model: "gpt-5",
        profiles: { fast: { vendor: "codex", model: "gpt-5-mini" } },
        envKeys: ["OPENAI_API_KEY"],
      },
      metadata: {
        ...configFixture().metadata,
        fields: {
          ...configFixture().metadata.fields,
          "agent.profiles": {
            source: "config-file",
            editable: false,
            applyMode: "restart",
          },
          "agent.profiles.fast.vendor": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
          "agent.profiles.fast.model": {
            source: "config-file",
            editable: true,
            applyMode: "hot",
          },
        },
      },
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== "/api/v1/config") {
        throw new Error(`unexpected request: ${String(input)}`);
      }
      if (init?.method === "PATCH") return response(initial);
      return response(initial);
    });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    expect(await screen.findByTestId("agent-profiles")).toBeTruthy();
    expect(
      screen.queryByText(
        "Agent profiles are read-only under the active config authority.",
      ),
    ).toBeNull();

    const modelInput = screen.getByLabelText(
      "agent.profiles.fast.model",
    ) as HTMLInputElement;
    expect(modelInput.disabled).toBe(false);
    fireEvent.change(modelInput, { target: { value: "gpt-5" } });

    fireEvent.change(screen.getByLabelText("New profile id"), {
      target: { value: "cheap" },
    });
    fireEvent.change(screen.getByLabelText("New profile vendor"), {
      target: { value: "opencode" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add profile" }));
    clickSaveChanges();

    fireEvent.click(
      await screen.findByRole("button", { name: "Apply changes" }),
    );

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PATCH")).toBe(
        true,
      );
    });
    const patchCall = fetchMock.mock.calls.find(([, init]) => init?.method === "PATCH");
    const body = JSON.parse(String(patchCall?.[1]?.body));
    expect(body.set["agent.profiles.fast.model"]).toBe("gpt-5");
    expect(body.set["agent.profiles.cheap.vendor"]).toBe("opencode");
  });
});
