import {
  ApiError,
  type ConfigData,
  type ConfigValue,
  type PatchConfigBody,
} from "./api";

export type ConfigFieldKind = "boolean" | "number" | "string" | "array";
export type ConfigDraft = string | boolean;

export type ConfigGroup = {
  id: string;
  title: string;
  description: string;
  accepts: (path: string) => boolean;
};

const SCHEDULER_PATHS = new Set([
  "scheduler.maxConcurrentRuns",
  "scheduler.slowLaneWarnThresholdMs",
]);

const agentPath = (path: string) =>
  path === "agent.vendor" ||
  path === "agent.model" ||
  path.startsWith("agent.timeouts.");

export const CONFIG_GROUPS: ConfigGroup[] = [
  {
    id: "scheduler",
    title: "Scheduler",
    description: "Concurrency and slow-lane diagnostics.",
    accepts: (path) => SCHEDULER_PATHS.has(path),
  },
  {
    id: "agent",
    title: "Agent",
    description: "Vendor, model, execution timeouts, and write-only environment values.",
    accepts: agentPath,
  },
  {
    id: "tools",
    title: "Runtime tools",
    description: "Hot-safe Looper and macOS notification executable paths.",
    accepts: (path) =>
      path === "tools.looperPath" || path === "tools.osascriptPath",
  },
  {
    id: "defaults",
    title: "Defaults & safety",
    description: "Publishing behavior and automation guardrails.",
    accepts: (path) => path.startsWith("defaults."),
  },
  {
    id: "notifications",
    title: "Notifications",
    description: "In-app and macOS notification policy.",
    accepts: (path) => path.startsWith("notifications."),
  },
  {
    id: "disclosure",
    title: "Disclosure",
    description: "Where automated work is visibly attributed.",
    accepts: (path) => path.startsWith("disclosure."),
  },
  {
    id: "instructions",
    title: "Instructions",
    description: "Global instruction discovery for newly claimed runs.",
    accepts: (path) => path.startsWith("instructions."),
  },
  {
    id: "roles",
    title: "Roles",
    description: "Global planner, worker, reviewer, fixer, and coordinator policy.",
    accepts: (path) => path.startsWith("roles."),
  },
];

const ARRAY_SUFFIXES = [
  ".labels",
  ".levels",
  ".soundForLevels",
  ".mentionOpenIds",
  ".mentionLogins",
  ".answerAuthors",
  ".slashCommands",
  ".allowedUsers",
  ".extraTransientErrorPatterns",
];

const BOOLEAN_NAMES = new Set([
  "enabled",
  "enabledByDefault",
  "autoDiscovery",
  "includeDrafts",
  "requireReviewRequest",
  "enableSelfReview",
  "includeReviewingLabel",
  "enhancedTransientClassification",
  "recoverExistingMatchedFailures",
  "stopOnApproved",
  "stopOnReadyLabel",
  "stopOnIdenticalOutput",
  "detectDuplicateFindings",
  "onHeadChange",
  "reReviewPromptOnHeadChange",
  "requireAuditComment",
  "requireNewHeadSinceThread",
  "requireCurrentReviewRequest",
  "requireBranchProtection",
  "requireAssigneeCurrentUser",
  "reTriageOnAuthorReply",
  "inApp",
  "includeAgent",
  "includeOS",
  "gitCommit",
  "pullRequest",
  "issueComment",
  "reviewComment",
  "inlineCommentVisible",
  "allowAutoCommit",
  "allowAutoPush",
  "allowAutoApprove",
  "allowRiskyFixes",
  "fixAllPullRequests",
]);

const SELECT_OPTIONS: Record<string, string[]> = {
  "agent.vendor": [
    "claude-code",
    "codex",
    "opencode",
    "cursor-cli",
    "grok-build",
  ],
  "defaults.openPrStrategy": ["all_done", "first_commit", "manual"],
  "defaults.addSnapshotMode": ["async", "full", "off"],
  "roles.coordinator.dispatch.mode": ["human-gated", "autonomous"],
  "roles.fixer.triggers.authorFilter": ["current_user", "any"],
  "roles.planner.triggers.labelMode": ["all", "any"],
  "roles.worker.triggers.labelMode": ["all", "any"],
  "roles.reviewer.discovery.triggers.labelMode": ["all", "any"],
  "roles.fixer.triggers.labelMode": ["all", "any"],
  "roles.reviewer.behavior.scope": [
    "full_pr",
    "changed_files",
    "changed_ranges",
  ],
  "roles.reviewer.behavior.publishMode": [
    "single_review",
    "summary_comment",
  ],
  "roles.reviewer.behavior.reviewEvents.clean": ["COMMENT", "APPROVE"],
  "roles.reviewer.behavior.reviewEvents.blocking": [
    "COMMENT",
    "REQUEST_CHANGES",
  ],
  "roles.reviewer.behavior.threadResolution.mode": [
    "report_only",
    "comment_only",
    "suggest_resolution",
    "resolve_objective",
  ],
  "roles.reviewer.behavior.threadResolution.scope": [
    "looper_authored_only",
  ],
  "roles.reviewer.behavior.threadResolution.autoResolve": ["objective_only"],
};

const HIGH_IMPACT_BOOLEAN_PATHS = new Set([
  "defaults.allowAutoCommit",
  "defaults.allowAutoPush",
  "defaults.allowRiskyFixes",
  "roles.planner.autoDiscovery",
  "roles.worker.autoDiscovery",
  "roles.reviewer.discovery.autoDiscovery",
  "roles.reviewer.discovery.triggers.enableSelfReview",
  "roles.fixer.autoDiscovery",
  "roles.coordinator.enabled",
  "roles.reviewer.behavior.threadResolution.enabled",
]);

const HIGH_IMPACT_SAFEGUARD_PATHS = new Set([
  "roles.reviewer.behavior.threadResolution.requireAuditComment",
  "roles.reviewer.behavior.threadResolution.requireNewHeadSinceThread",
  "roles.reviewer.behavior.threadResolution.requireCurrentReviewRequest",
]);

const THREAD_RESOLUTION_MODE_PATH =
  "roles.reviewer.behavior.threadResolution.mode";

function isConfigObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function getConfigValue(data: ConfigData, path: string): unknown {
  let current: unknown = data;
  for (const segment of path.split(".")) {
    if (!isConfigObject(current)) return undefined;
    current = current[segment];
  }
  return current;
}

function flattenLeaves(
  value: unknown,
  prefix: string,
  target: Set<string>,
): void {
  if (Array.isArray(value) || value === null || typeof value !== "object") {
    if (prefix) target.add(prefix);
    return;
  }
  for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
    if (prefix === "agent" && (key === "envKeys" || key === "paramsConfigured")) {
      continue;
    }
    flattenLeaves(child, prefix ? `${prefix}.${key}` : key, target);
  }
}

export function configFieldPaths(data: ConfigData, group: ConfigGroup): string[] {
  const paths = new Set<string>();
  for (const path of Object.keys(data.metadata.fields ?? {})) {
    if (group.accepts(path)) paths.add(path);
  }
  flattenLeaves(data[group.id], group.id, paths);
  return [...paths]
    .filter(group.accepts)
    .filter((path) => data.metadata.fields[path]?.applyMode === "hot")
    .filter((path) => !path.startsWith("agent.env") && path !== "agent.params")
    .sort((a, b) => a.localeCompare(b));
}

export function configFieldKind(
  path: string,
  effectiveValue: unknown,
): ConfigFieldKind {
  if (Array.isArray(effectiveValue) || ARRAY_SUFFIXES.some((s) => path.endsWith(s))) {
    return "array";
  }
  if (typeof effectiveValue === "boolean") return "boolean";
  if (typeof effectiveValue === "number") return "number";
  if (BOOLEAN_NAMES.has(path.split(".").at(-1) ?? "")) return "boolean";
  return "string";
}

export function configSelectOptions(path: string): string[] | undefined {
  return SELECT_OPTIONS[path];
}

export function draftFromValue(kind: ConfigFieldKind, value: unknown): ConfigDraft {
  if (kind === "boolean") return value === true;
  if (kind === "array") {
    return Array.isArray(value) ? value.map(String).join("\n") : "";
  }
  return value == null ? "" : String(value);
}

function parseDraft(
  kind: ConfigFieldKind,
  draft: ConfigDraft,
): { value?: ConfigValue; error?: string } {
  if (kind === "boolean") return { value: draft === true };
  const raw = String(draft);
  if (kind === "number") {
    if (!raw.trim()) return { error: "Enter a whole number." };
    const value = Number(raw);
    if (!Number.isFinite(value) || !Number.isInteger(value)) {
      return { error: "Enter a whole number." };
    }
    return { value };
  }
  if (kind === "array") {
    return {
      value: raw
        .split(/\n/)
        .map((item) => item.trim())
        .filter(Boolean),
    };
  }
  return { value: raw };
}

function valuesEqual(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

export function buildConfigPatch(
  data: ConfigData,
  drafts: Record<string, ConfigDraft>,
  unsetPaths: Iterable<string>,
  secretSet: Record<string, string> = {},
): { body: PatchConfigBody; errors: Record<string, string> } {
  const set: Record<string, ConfigValue> = {};
  const errors: Record<string, string> = {};
  const unset = new Set(unsetPaths);

  for (const [path, draft] of Object.entries(drafts)) {
    if (unset.has(path)) continue;
    const kind = configFieldKind(path, getConfigValue(data, path));
    const parsed = parseDraft(kind, draft);
    if (parsed.error) {
      errors[path] = parsed.error;
      continue;
    }
    if (!valuesEqual(parsed.value, getConfigValue(data, path))) {
      set[path] = parsed.value as ConfigValue;
    }
  }

  for (const [path, value] of Object.entries(secretSet)) {
    if (!unset.has(path)) set[path] = value;
  }

  return {
    body: { revision: data.metadata.revision, set, unset: [...unset].sort() },
    errors,
  };
}

export type HighImpactChange = { path: string; label: string };

export function highImpactChanges(
  data: ConfigData,
  set: Record<string, ConfigValue>,
  unset: Iterable<string> = [],
): HighImpactChange[] {
  const changes: HighImpactChange[] = [];
  for (const [path, value] of Object.entries(set)) {
    if (path === "agent.vendor" && getConfigValue(data, path) !== value) {
      changes.push({ path, label: `Agent vendor → ${String(value)}` });
      continue;
    }
    if (
      path === "roles.coordinator.dispatch.mode" &&
      value === "autonomous" &&
      getConfigValue(data, path) !== value
    ) {
      changes.push({ path, label: "Coordinator dispatch → autonomous" });
      continue;
    }
    if (
      HIGH_IMPACT_BOOLEAN_PATHS.has(path) &&
      value === true &&
      getConfigValue(data, path) !== true
    ) {
      changes.push({ path, label: configFieldLabel(path) });
      continue;
    }
    if (
      HIGH_IMPACT_SAFEGUARD_PATHS.has(path) &&
      value === false &&
      getConfigValue(data, path) !== false
    ) {
      changes.push({
        path,
        label: `Disable ${configFieldLabel(path)}`,
      });
      continue;
    }
    if (
      path === "roles.fixer.triggers.authorFilter" &&
      value === "any" &&
      getConfigValue(data, path) !== value
    ) {
      changes.push({ path, label: "Fixer author filter → any author" });
      continue;
    }
    if (
      path === "roles.reviewer.behavior.reviewEvents.clean" &&
      value === "APPROVE" &&
      getConfigValue(data, path) !== value
    ) {
      changes.push({ path, label: "Reviewer clean event → APPROVE" });
      continue;
    }
    if (
      path === "roles.reviewer.behavior.reviewEvents.blocking" &&
      value === "REQUEST_CHANGES" &&
      getConfigValue(data, path) !== value
    ) {
      changes.push({ path, label: "Reviewer blocking event → REQUEST_CHANGES" });
      continue;
    }
    if (
      path === THREAD_RESOLUTION_MODE_PATH &&
      value === "resolve_objective" &&
      getConfigValue(data, path) !== value
    ) {
      changes.push({ path, label: "Reviewer thread resolution → resolve objective" });
    }
  }
  for (const path of unset) {
    const current = getConfigValue(data, path);
    if (path === "agent.vendor") {
      changes.push({
        path,
        label: "Unset agent vendor (new work may stop until another authority supplies one)",
      });
      continue;
    }
    if (path === "roles.coordinator.dispatch.mode") {
      changes.push({
        path,
        label: "Unset coordinator dispatch mode (the inherited mode will become active)",
      });
      continue;
    }
    if (HIGH_IMPACT_BOOLEAN_PATHS.has(path) && current !== true) {
      changes.push({
        path,
        label: `Unset ${configFieldLabel(path)} (inherited value may enable it)`,
      });
      continue;
    }
    if (HIGH_IMPACT_SAFEGUARD_PATHS.has(path) && current !== false) {
      changes.push({
        path,
        label: `Unset ${configFieldLabel(path)} (the inherited value may disable this safeguard)`,
      });
      continue;
    }
    if (path === "roles.fixer.triggers.authorFilter" && current !== "any") {
      changes.push({
        path,
        label: "Unset fixer author filter (the inherited value may include any author)",
      });
      continue;
    }
    if (
      path === "roles.reviewer.behavior.reviewEvents.clean" &&
      current !== "APPROVE"
    ) {
      changes.push({
        path,
        label: "Unset reviewer clean event (inherited value may approve)",
      });
      continue;
    }
    if (
      path === "roles.reviewer.behavior.reviewEvents.blocking" &&
      current !== "REQUEST_CHANGES"
    ) {
      changes.push({
        path,
        label:
          "Unset reviewer blocking event (inherited value may request changes)",
      });
      continue;
    }
    if (path === THREAD_RESOLUTION_MODE_PATH && current !== "resolve_objective") {
      changes.push({
        path,
        label: "Unset reviewer thread resolution mode (inherited mode may resolve threads)",
      });
    }
  }
  return changes;
}

function titleWord(value: string): string {
  return value
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}

export function configFieldLabel(path: string): string {
  return path
    .split(".")
    .slice(1)
    .map(titleWord)
    .join(" · ");
}

type FieldIssue = { path?: unknown; message?: unknown };

function formFieldPath(path: string): string {
  // Config validation may identify a particular list element. The dashboard
  // edits arrays atomically, so attach that issue to the whole array control.
  return path.replace(/\[\d+\]/g, "");
}

function addIssues(target: Record<string, string>, value: unknown): void {
  if (!Array.isArray(value)) return;
  for (const issue of value as FieldIssue[]) {
    if (typeof issue?.path === "string" && typeof issue?.message === "string") {
      target[formFieldPath(issue.path)] = issue.message;
    }
  }
}

/** Accept the envelope detail shapes used by current and older API handlers. */
export function configFieldErrors(error: unknown): Record<string, string> {
  if (!(error instanceof ApiError) || !isConfigObject(error.details)) return {};
  const result: Record<string, string> = {};
  const details = error.details;
  for (const key of ["fields", "fieldErrors"]) {
    const value = details[key];
    if (isConfigObject(value)) {
      for (const [path, message] of Object.entries(value)) {
        if (typeof message === "string") result[formFieldPath(path)] = message;
      }
    }
  }
  addIssues(result, details.issues);
  addIssues(result, details.errors);
  return result;
}
