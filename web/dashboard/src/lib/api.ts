const TOKEN_KEY = "looper.dashboard.token";

export type ApiEnvelope<T> = {
  ok: boolean;
  data?: T;
  error?: {
    code?: string;
    message?: string;
    details?: unknown;
  } | null;
  requestId?: string;
};

export class ApiError extends Error {
  status: number;
  code?: string;
  requestId?: string;
  details?: unknown;

  constructor(
    message: string,
    opts: {
      status: number;
      code?: string;
      requestId?: string;
      details?: unknown;
    },
  ) {
    super(message);
    this.name = "ApiError";
    this.status = opts.status;
    this.code = opts.code;
    this.requestId = opts.requestId;
    this.details = opts.details;
  }
}

export function getDashboardToken(): string | null {
  try {
    return sessionStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setDashboardToken(token: string): void {
  sessionStorage.setItem(TOKEN_KEY, token);
}

export function clearDashboardToken(): void {
  sessionStorage.removeItem(TOKEN_KEY);
}

export async function apiFetch<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) {
    headers.set("Accept", "application/json");
  }
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const token = getDashboardToken();
  if (token && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const response = await fetch(path, { ...init, headers });

  let envelope: ApiEnvelope<T> | null = null;
  const contentType = response.headers.get("content-type") ?? "";
  if (contentType.includes("application/json")) {
    try {
      envelope = (await response.json()) as ApiEnvelope<T>;
    } catch {
      envelope = null;
    }
  }

  if (!response.ok || envelope?.ok === false) {
    const message =
      envelope?.error?.message ||
      `Request failed (${response.status} ${response.statusText})`;
    throw new ApiError(message, {
      status: response.status,
      code: envelope?.error?.code,
      requestId: envelope?.requestId,
      details: envelope?.error?.details,
    });
  }

  // Success requires a well-formed JSON envelope: ok === true and data defined.
  if (!envelope || envelope.ok !== true || !("data" in envelope) || envelope.data === undefined) {
    throw new ApiError(
      envelope
        ? "Malformed success envelope (ok/data missing)"
        : `Expected JSON success envelope (${response.status})`,
      {
        status: response.status,
        code: envelope?.error?.code,
        requestId: envelope?.requestId,
      },
    );
  }

  return envelope.data as T;
}

export type HealthzData = {
  healthy: boolean;
  startedAt?: string;
  storage?: {
    ok?: boolean;
    mode?: string;
    dbPath?: string;
  };
};

export type LoopRoleCounts = {
  queued?: number;
  running?: number;
  waiting?: number;
  paused?: number;
  failed?: number;
  terminated?: number;
  stopped?: number;
};

export type StatusData = {
  service?: {
    healthy?: boolean;
    version?: string;
    daemonMode?: string;
    startedAt?: string;
  };
  scheduler?: {
    healthy?: boolean;
    queuedItems?: number;
    runningItems?: number;
    activeRuns?: number;
    totalRuns?: number;
    failedItems?: number;
  };
  loops?: Record<string, LoopRoleCounts>;
  storage?: {
    healthy?: boolean;
    mode?: string;
    dbPath?: string;
  };
  agent?: {
    vendor?: string;
  };
};

export type BootstrapExchangeData = {
  token?: string;
  accessToken?: string;
};

export type ActiveRunTarget = {
  type: string;
  projectId?: string | null;
  repo?: string | null;
  prNumber?: number | null;
  issueNumber?: number | null;
  label: string;
};

export type ActiveRunAgent = {
  active: boolean;
  activeCount: number;
  executionId: string;
  vendor: string;
  pid?: number | null;
  startedAt: string;
  lastHeartbeatAt?: string | null;
  heartbeatCount: number;
  status: string;
};

export type ActiveRunWorktree = {
  id?: string | null;
  path: string;
  branch?: string | null;
};

export type ActiveRun = {
  seq: number;
  runId?: string | null;
  loopId: string;
  projectId: string;
  type: string;
  status: string;
  loopStatus: string;
  displayStatus: string;
  /** Current attempt count from latest queue item when present. */
  attempts?: number | null;
  /** Max attempts (-1 = unlimited) from latest queue item when present. */
  maxAttempts?: number | null;
  lastFailureKind?: string | null;
  lastFailureReason?: string | null;
  resumePolicy?: string | null;
  currentStep?: string | null;
  startedAt?: string | null;
  endedAt?: string | null;
  target: ActiveRunTarget;
  agent?: ActiveRunAgent | null;
  worktree?: ActiveRunWorktree | null;
};

export type ActiveRunsList = {
  items: ActiveRun[];
};

export type Loop = {
  id: string;
  seq: number;
  projectId: string;
  type: string;
  targetType: string;
  targetId?: string | null;
  repo?: string | null;
  prNumber?: number | null;
  status: string;
  configJson?: string | null;
  metadataJson?: string | null;
  lastRunAt?: string | null;
  nextRunAt?: string | null;
  createdAt: string;
  updatedAt: string;
  /** Current attempt count from latest queue item when present. */
  attempts?: number | null;
  /** Max attempts (-1 = unlimited) from latest queue item when present. */
  maxAttempts?: number | null;
  lastFailureKind?: string | null;
  lastFailureReason?: string | null;
};

export type LoopsList = {
  items: Loop[];
  total: number;
  limit?: number | null;
  offset?: number;
};

export type Project = {
  id: string;
  name: string;
  repoPath: string;
  baseBranch: string;
  archived: boolean;
  provider: string;
  repo?: string | null;
  worktreeRoot?: string | null;
  createdAt: string;
  updatedAt: string;
};

export type ProjectsList = {
  items: Project[];
};

export type ConfigScalar = string | number | boolean | null;
export type ConfigValue =
  | ConfigScalar
  | ConfigValue[]
  | { [key: string]: ConfigValue | undefined };

export type ConfigFieldMetadata = {
  source: "default" | "config-file" | "env" | "cli" | string;
  editable: boolean;
  applyMode: "hot" | "restart" | string;
};

export type ConfigMetadata = {
  configPath: string;
  format: string;
  filePresent: boolean;
  revision: string;
  lastAttemptAt?: string | null;
  lastAppliedAt?: string | null;
  lastError?: string | null;
  rejectedPaths?: string[];
  fields: Record<string, ConfigFieldMetadata>;
};

export type ConfigAgentProfileView = {
  vendor?: string | null;
  model?: string | null;
};

export type ConfigAgentView = {
  vendor?: string | null;
  model?: string | null;
  /** Named vendor/model profiles (no params). */
  profiles?: Record<string, ConfigAgentProfileView>;
  nativeResume?: { enabled?: boolean };
  timeouts?: Record<string, number>;
  /** Secret-safe projection: values are never returned. */
  envKeys?: string[];
};

/**
 * Effective config view. The backend deliberately omits startup-only sections
 * from dashboard editing metadata and projects remain managed by their own API.
 */
export type ConfigData = {
  metadata: ConfigMetadata;
  scheduler?: Record<string, ConfigValue | undefined>;
  agent?: ConfigAgentView;
  tools?: Record<string, ConfigValue | undefined>;
  defaults?: Record<string, ConfigValue | undefined>;
  notifications?: Record<string, ConfigValue | undefined>;
  disclosure?: Record<string, ConfigValue | undefined>;
  instructions?: Record<string, ConfigValue | undefined>;
  hitl?: Record<string, ConfigValue | undefined>;
  roles?: Record<string, ConfigValue | undefined>;
  [key: string]: ConfigValue | ConfigAgentView | ConfigMetadata | undefined;
};

export type PatchConfigBody = {
  revision: string;
  set: Record<string, ConfigValue>;
  unset: string[];
};

export type LoopLogsRun = {
  runId: string;
  status: string;
  currentStep?: string | null;
  startedAt: string;
  endedAt?: string | null;
  summary?: string | null;
  errorMessage?: string | null;
};

export type LoopLogsAgent = {
  executionId: string;
  vendor: string;
  status: string;
  pid?: number | null;
  startedAt: string;
  endedAt?: string | null;
  heartbeatCount: number;
  lastHeartbeatAt?: string | null;
  summary?: string | null;
  parseStatus?: string | null;
  errorMessage?: string | null;
  stdout: string;
  stderr: string;
};

export type LoopLogsSnapshot = {
  seq: number;
  loopId: string;
  loopType: string;
  loopStatus: string;
  run?: LoopLogsRun | null;
  agent?: LoopLogsAgent | null;
};

export type LoopLogsChunk = {
  runId?: string | null;
  currentStep?: string | null;
  executionId?: string | null;
  vendor?: string | null;
  pid?: number | null;
  status?: string | null;
  content: string;
};

export type LoopLogsEnd = {
  reason?: string;
};

/** Exchange one-shot bootstrap code for a session token when present in the URL. */
export async function exchangeBootstrapCodeIfPresent(): Promise<void> {
  const url = new URL(window.location.href);
  const code = url.searchParams.get("code");
  if (!code) {
    return;
  }

  try {
    const data = await apiFetch<BootstrapExchangeData>(
      "/api/v1/dashboard/bootstrap/exchange",
      {
        method: "POST",
        body: JSON.stringify({ code }),
      },
    );
    const token = data?.token ?? data?.accessToken;
    if (token) {
      setDashboardToken(token);
    }
  } finally {
    url.searchParams.delete("code");
    const next =
      url.pathname +
      (url.searchParams.toString() ? `?${url.searchParams.toString()}` : "") +
      url.hash;
    window.history.replaceState({}, "", next);
  }
}

export function fetchHealthz(signal?: AbortSignal): Promise<HealthzData> {
  return apiFetch<HealthzData>("/api/v1/healthz", { signal });
}

export function fetchStatus(signal?: AbortSignal): Promise<StatusData> {
  return apiFetch<StatusData>("/api/v1/status", { signal });
}

export function fetchActiveRuns(signal?: AbortSignal): Promise<ActiveRunsList> {
  return apiFetch<ActiveRunsList>("/api/v1/runs/active", { signal });
}

export function fetchLoops(opts?: {
  status?: string;
  projectId?: string;
  limit?: number;
  offset?: number;
  signal?: AbortSignal;
}): Promise<LoopsList> {
  const params = new URLSearchParams();
  if (opts?.status) params.set("status", opts.status);
  if (opts?.projectId) params.set("projectId", opts.projectId);
  if (opts?.limit != null) params.set("limit", String(opts.limit));
  if (opts?.offset != null) params.set("offset", String(opts.offset));
  const qs = params.toString();
  return apiFetch<LoopsList>(`/api/v1/loops${qs ? `?${qs}` : ""}`, {
    signal: opts?.signal,
  });
}

export function fetchLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<Loop> {
  return apiFetch<Loop>(`/api/v1/loops/${encodeURIComponent(selector)}`, {
    signal,
  });
}

export function fetchProjects(signal?: AbortSignal): Promise<ProjectsList> {
  return apiFetch<ProjectsList>("/api/v1/projects", { signal });
}

export function fetchConfig(signal?: AbortSignal): Promise<ConfigData> {
  return apiFetch<ConfigData>("/api/v1/config", { signal });
}

export function patchConfig(
  body: PatchConfigBody,
  signal?: AbortSignal,
): Promise<ConfigData> {
  return apiFetch<ConfigData>("/api/v1/config", {
    method: "PATCH",
    body: JSON.stringify(body),
    signal,
  });
}

// --- Loop / run mutations (operator dashboard) ---

export type RetryLoopBody = {
  mode: "auto";
  resetAttempts: true;
  /** Never set on handback; optional on retry only. */
  discardWorktreeChanges?: boolean;
};

export type RetryLoopResult = {
  loop: Loop;
  queueItemId?: string | null;
  mode: string;
  resetAttempts: boolean;
  discardWorktreeChanges: boolean;
  worktreeDiscard?: unknown;
};

export type StopActiveRunResult = {
  stopped: boolean;
  loopId: string;
};

export type TakeoverResult = {
  loopId: string;
  vendor?: string;
  sessionId?: string;
  worktreePath?: string;
  supported: boolean;
  resumeCommand?: string;
  message?: string;
};

const RETRY_BODY: RetryLoopBody = {
  mode: "auto",
  resetAttempts: true,
};

/** Unpause: POST /loops/{sel}/start (not CLI interactive resume). */
export function startLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<Loop> {
  return apiFetch<Loop>(
    `/api/v1/loops/${encodeURIComponent(selector)}/start`,
    { method: "POST", signal },
  );
}

export function pauseLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<Loop> {
  return apiFetch<Loop>(
    `/api/v1/loops/${encodeURIComponent(selector)}/pause`,
    { method: "POST", signal },
  );
}

export function retryLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<RetryLoopResult> {
  return apiFetch<RetryLoopResult>(
    `/api/v1/loops/${encodeURIComponent(selector)}/retry`,
    {
      method: "POST",
      body: JSON.stringify(RETRY_BODY),
      signal,
    },
  );
}

export function stopActiveRun(
  selector: string,
  signal?: AbortSignal,
): Promise<StopActiveRunResult> {
  return apiFetch<StopActiveRunResult>(
    `/api/v1/runs/active/${encodeURIComponent(selector)}/stop`,
    { method: "POST", signal },
  );
}

export function takeoverLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<TakeoverResult> {
  return apiFetch<TakeoverResult>(
    `/api/v1/loops/${encodeURIComponent(selector)}/takeover`,
    { method: "POST", signal },
  );
}

/** Handback: retry-shaped body without discardWorktreeChanges. */
export function handbackLoop(
  selector: string,
  signal?: AbortSignal,
): Promise<RetryLoopResult> {
  return apiFetch<RetryLoopResult>(
    `/api/v1/loops/${encodeURIComponent(selector)}/handback`,
    {
      method: "POST",
      body: JSON.stringify(RETRY_BODY),
      signal,
    },
  );
}

/** Build path for loop logs SSE follow stream (`follow=1`, optional `stderr=1`). */
export function loopLogsFollowPath(
  selector: string,
  opts?: { stderr?: boolean },
): string {
  const params = new URLSearchParams({ follow: "1" });
  if (opts?.stderr) {
    params.set("stderr", "1");
  }
  return `/api/v1/loops/${encodeURIComponent(selector)}/logs?${params.toString()}`;
}

/**
 * Open loop logs SSE stream (follow=1).
 * Pass `{ stderr: true }` to follow agent stderr; default follows stdout
 * (server may fall back to stderr when stdout is empty).
 * Caller must parse body with sse.ts.
 */
export async function openLoopLogsStream(
  selector: string,
  signal?: AbortSignal,
  opts?: { stderr?: boolean },
): Promise<Response> {
  const headers = new Headers({
    Accept: "text/event-stream",
  });
  const token = getDashboardToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const response = await fetch(loopLogsFollowPath(selector, opts), {
    headers,
    signal,
  });

  if (!response.ok) {
    let message = `Request failed (${response.status} ${response.statusText})`;
    const contentType = response.headers.get("content-type") ?? "";
    if (contentType.includes("application/json")) {
      try {
        const envelope = (await response.json()) as ApiEnvelope<unknown>;
        if (envelope?.error?.message) {
          message = envelope.error.message;
        }
        throw new ApiError(message, {
          status: response.status,
          code: envelope?.error?.code,
          requestId: envelope?.requestId,
        });
      } catch (err) {
        if (err instanceof ApiError) throw err;
      }
    }
    throw new ApiError(message, { status: response.status });
  }

  const contentType = response.headers.get("content-type") ?? "";
  // Accept "text/event-stream" and "text/event-stream; charset=utf-8".
  if (!contentType.toLowerCase().startsWith("text/event-stream")) {
    throw new ApiError(
      `Expected text/event-stream, got ${contentType || "(missing Content-Type)"}`,
      { status: response.status },
    );
  }

  return response;
}
