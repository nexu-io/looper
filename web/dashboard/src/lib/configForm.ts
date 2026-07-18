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

/** Coding roles that accept optional agent profile/vendor/model bindings. */
export const CODING_ROLES = ["planner", "worker", "reviewer", "fixer"] as const;
export type CodingRole = (typeof CODING_ROLES)[number];

export const ROLE_AGENT_FIELDS = ["profile", "vendor", "model"] as const;
export type RoleAgentField = (typeof ROLE_AGENT_FIELDS)[number];

export const AGENT_VENDOR_OPTIONS = [
  "claude-code",
  "codex",
  "opencode",
  "cursor-cli",
  "grok-build",
] as const;

const agentProfileLeafPath = /^agent\.profiles\.[A-Za-z0-9_-]+\.(vendor|model)$/;
const roleAgentLeafPath =
  /^roles\.(planner|worker|reviewer|fixer)\.agent\.(profile|vendor|model)$/;

const agentProfileWholePath = /^agent\.profiles\.[A-Za-z0-9_-]+$/;

export function isAgentProfileLeafPath(path: string): boolean {
  return agentProfileLeafPath.test(path);
}

export function isAgentProfileWholePath(path: string): boolean {
  return agentProfileWholePath.test(path);
}

/** Profile leaf, whole-profile, or coding-role agent binding path. */
export function isCuratedAgentIdentityPath(path: string): boolean {
  return (
    isAgentProfileLeafPath(path) ||
    isAgentProfileWholePath(path) ||
    isRoleAgentLeafPath(path)
  );
}

export function isRoleAgentLeafPath(path: string): boolean {
  return roleAgentLeafPath.test(path);
}

export function roleAgentPath(role: CodingRole, field: RoleAgentField): string {
  return `roles.${role}.agent.${field}`;
}

export function agentProfilePath(
  id: string,
  field: "vendor" | "model",
): string {
  return `agent.profiles.${id}.${field}`;
}

export function isValidAgentProfileId(id: string): boolean {
  return /^[A-Za-z0-9_-]+$/.test(id);
}

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
    description:
      "Vendor, model, profiles, execution timeouts, and write-only environment values.",
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
    description:
      "Global planner, worker, reviewer, fixer, and coordinator policy, including optional agent profile/vendor/model bindings.",
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
  "agent.vendor": [...AGENT_VENDOR_OPTIONS],
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
    if (
      prefix === "agent" &&
      (key === "envKeys" || key === "paramsConfigured" || key === "profiles")
    ) {
      continue;
    }
    // Role agent bindings are curated fixed leaves, not free-form nested maps.
    if (
      /^roles\.(planner|worker|reviewer|fixer)$/.test(prefix) &&
      key === "agent"
    ) {
      continue;
    }
    flattenLeaves(child, prefix ? `${prefix}.${key}` : key, target);
  }
}

function injectCuratedRoleAgentPaths(paths: Set<string>): void {
  for (const role of CODING_ROLES) {
    for (const field of ROLE_AGENT_FIELDS) {
      paths.add(roleAgentPath(role, field));
    }
  }
}

function isHotEditableField(data: ConfigData, path: string): boolean {
  const meta = data.metadata.fields[path];
  if (meta?.applyMode === "hot") return true;
  // Curated role-agent leaves are always hot when not explicitly restart-bound.
  // They are injected even when absent from the published snapshot so operators
  // can set the first binding without a prior file value.
  if (isRoleAgentLeafPath(path)) {
    return meta?.applyMode !== "restart";
  }
  return false;
}

export function configFieldPaths(data: ConfigData, group: ConfigGroup): string[] {
  const paths = new Set<string>();
  for (const path of Object.keys(data.metadata.fields ?? {})) {
    if (group.accepts(path)) paths.add(path);
  }
  flattenLeaves(data[group.id], group.id, paths);
  if (group.id === "roles") injectCuratedRoleAgentPaths(paths);
  return [...paths]
    .filter(group.accepts)
    .filter((path) => isHotEditableField(data, path))
    .filter(
      (path) =>
        !path.startsWith("agent.env") &&
        path !== "agent.params" &&
        path !== "agent.profiles" &&
        !path.startsWith("agent.profiles.") &&
        !/^roles\.(planner|worker|reviewer|fixer)\.agent$/.test(path),
    )
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
  if (SELECT_OPTIONS[path]) return SELECT_OPTIONS[path];
  if (
    path === "agent.vendor" ||
    isAgentProfileLeafPath(path) && path.endsWith(".vendor") ||
    isRoleAgentLeafPath(path) && path.endsWith(".vendor")
  ) {
    return [...AGENT_VENDOR_OPTIONS];
  }
  return undefined;
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

/**
 * Whether a profile identity leaf value counts as present.
 * Vendor: nullish/empty means absent.
 * Model: empty string is a valid suppression binding (backend non-nil empty);
 * only nullish means absent.
 */
function profileIdentityValuePresent(
  field: "vendor" | "model",
  value: unknown,
): boolean {
  if (value == null) return false;
  if (field === "model") return true;
  return String(value).trim() !== "";
}

/**
 * True when an identity leaf would remain after applying set/unset.
 * Empty model strings count as present (model-suppression binding).
 */
function profileLeafPresentAfterPatch(
  data: ConfigData,
  set: Record<string, ConfigValue>,
  unset: Set<string>,
  profileId: string,
  field: "vendor" | "model",
): boolean {
  const wholePath = `agent.profiles.${profileId}`;
  if (unset.has(wholePath)) return false;
  const path = agentProfilePath(profileId, field);
  if (unset.has(path)) return false;
  if (Object.hasOwn(set, path)) {
    return profileIdentityValuePresent(field, set[path]);
  }
  return profileIdentityValuePresent(field, getConfigValue(data, path));
}

/**
 * Backend validateAgentProfiles rejects agent.profiles.<id> = {}. Promote leaf
 * unsets that would empty a published profile into whole-profile removal, and
 * drop leaf ops for unpublished empty profiles so the patch never stages {}.
 */
function collapseEmptyProfileLeafOps(
  data: ConfigData,
  set: Record<string, ConfigValue>,
  unset: Set<string>,
): void {
  const profileIds = new Set<string>();
  for (const path of unset) {
    const whole = /^agent\.profiles\.([A-Za-z0-9_-]+)$/.exec(path);
    if (whole) profileIds.add(whole[1]);
    const leaf = /^agent\.profiles\.([A-Za-z0-9_-]+)\.(vendor|model)$/.exec(path);
    if (leaf) profileIds.add(leaf[1]);
  }
  for (const path of Object.keys(set)) {
    const leaf = /^agent\.profiles\.([A-Za-z0-9_-]+)\.(vendor|model)$/.exec(path);
    if (leaf) profileIds.add(leaf[1]);
  }
  for (const id of profileIds) {
    const wholePath = `agent.profiles.${id}`;
    const vendorPath = agentProfilePath(id, "vendor");
    const modelPath = agentProfilePath(id, "model");
    if (unset.has(wholePath)) {
      unset.delete(vendorPath);
      unset.delete(modelPath);
      delete set[vendorPath];
      delete set[modelPath];
      continue;
    }
    const hasVendor = profileLeafPresentAfterPatch(
      data,
      set,
      unset,
      id,
      "vendor",
    );
    const hasModel = profileLeafPresentAfterPatch(
      data,
      set,
      unset,
      id,
      "model",
    );
    if (hasVendor || hasModel) continue;

    const published = data.agent?.profiles?.[id];
    const profileExists = published != null && typeof published === "object";
    unset.delete(vendorPath);
    unset.delete(modelPath);
    delete set[vendorPath];
    delete set[modelPath];
    if (profileExists) {
      unset.add(wholePath);
    }
  }
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
    // Profile/role identity text leaves: empty draft means inherit (omit leaf),
    // not set "". Role .profile must unset rather than stage "" — backend
    // validateRoleAgentBindings rejects empty profile when sibling vendor/model
    // keeps the role agent object alive. Model empty-string suppress is not
    // staged from the text control; use Unset or an explicit future control.
    //
    // Callers that probe single-path drafts (Config onDraft / rebase) must treat
    // an unset-only result as a staged change via draftStagesConfigChange so
    // empty drafts are retained until Save — otherwise the field snaps back.
    if (
      parsed.value === "" &&
      (isAgentProfileLeafPath(path) || isRoleAgentLeafPath(path)) &&
      (path.endsWith(".model") || path.endsWith(".profile"))
    ) {
      const current = getConfigValue(data, path);
      if (current != null && current !== "") {
        unset.add(path);
      }
      continue;
    }
    if (!valuesEqual(parsed.value, getConfigValue(data, path))) {
      set[path] = parsed.value as ConfigValue;
    }
  }

  for (const [path, value] of Object.entries(secretSet)) {
    if (!unset.has(path)) set[path] = value;
  }

  // Avoid agent.profiles.<id>={} which validateAgentProfiles rejects.
  collapseEmptyProfileLeafOps(data, set, unset);

  return {
    body: { revision: data.metadata.revision, set, unset: [...unset].sort() },
    errors,
  };
}

/**
 * Whether a single-path draft would stage a set, unset (including whole-profile
 * collapse of the last profile leaf), or validation error. Used by Config onDraft
 * and pending-rebase reconciliation so unset-only empty profile/role drafts are
 * retained instead of discarded when buildConfigPatch emits no set/error.
 */
export function draftStagesConfigChange(
  data: ConfigData,
  path: string,
  draft: ConfigDraft,
): boolean {
  const candidate = buildConfigPatch(data, { [path]: draft }, []);
  if (Object.hasOwn(candidate.errors, path)) return true;
  if (Object.hasOwn(candidate.body.set, path)) return true;
  if (candidate.body.unset.includes(path)) return true;
  // Dual empty profile leaves collapse to agent.profiles.<id> unset.
  if (isAgentProfileLeafPath(path)) {
    const wholePath = path.replace(/\.(vendor|model)$/, "");
    if (candidate.body.unset.includes(wholePath)) return true;
  }
  return false;
}

/**
 * Whether unsetting (or clearing) this profile leaf would leave the profile
 * with no vendor and no model. Used by the dashboard to promote last-leaf
 * unsets to whole-profile removal instead of staging a doomed empty object.
 *
 * Empty-string model is treated as present: backend non-nil empty model
 * suppresses inherited/params models, so unsetting only vendor must leave
 * `{model: ""}` rather than removing the whole profile.
 */
export function profileLeafUnsetWouldEmpty(
  data: ConfigData,
  drafts: Record<string, ConfigDraft>,
  unsetPaths: Iterable<string>,
  profileId: string,
  field: "vendor" | "model",
): boolean {
  const wholePath = `agent.profiles.${profileId}`;
  const unset = new Set(unsetPaths);
  if (unset.has(wholePath)) return false;

  const otherField: "vendor" | "model" =
    field === "vendor" ? "model" : "vendor";
  const otherPath = agentProfilePath(profileId, otherField);
  if (unset.has(otherPath)) return true;

  if (Object.hasOwn(drafts, otherPath)) {
    const draft = drafts[otherPath];
    const trimmed = String(draft ?? "").trim();
    if (otherField === "model") {
      // Non-empty draft keeps a model. Empty draft stages inherit-unset only
      // when the published model is non-empty; published "" suppress remains.
      if (trimmed !== "") return false;
      return !profileIdentityValuePresent(
        "model",
        getConfigValue(data, otherPath),
      );
    }
    return trimmed === "";
  }
  return !profileIdentityValuePresent(
    otherField,
    getConfigValue(data, otherPath),
  );
}

export type HighImpactChange = { path: string; label: string };

function highImpactVendorLabel(path: string, value: unknown): string {
  if (path === "agent.vendor") return `Agent vendor → ${String(value)}`;
  const roleVendor = /^roles\.(planner|worker|reviewer|fixer)\.agent\.vendor$/.exec(
    path,
  );
  if (roleVendor) {
    return `${titleWord(roleVendor[1])} agent vendor → ${String(value)}`;
  }
  const profileVendor = /^agent\.profiles\.([A-Za-z0-9_-]+)\.vendor$/.exec(path);
  if (profileVendor) {
    return `Profile ${profileVendor[1]} vendor → ${String(value)}`;
  }
  return `${configFieldLabel(path)} → ${String(value)}`;
}

function isHighImpactVendorPath(path: string): boolean {
  return (
    path === "agent.vendor" ||
    /^roles\.(planner|worker|reviewer|fixer)\.agent\.vendor$/.test(path) ||
    /^agent\.profiles\.[A-Za-z0-9_-]+\.vendor$/.test(path)
  );
}

function isHighImpactProfileSwitchPath(path: string): boolean {
  return /^roles\.(planner|worker|reviewer|fixer)\.agent\.profile$/.test(path);
}

function profileReferencedByRoles(data: ConfigData, profileId: string): string[] {
  const roles: string[] = [];
  for (const role of CODING_ROLES) {
    const binding = (data.roles as Record<string, { agent?: { profile?: string } }> | undefined)?.[
      role
    ]?.agent?.profile;
    if (typeof binding === "string" && binding === profileId) {
      roles.push(role);
    }
  }
  return roles;
}

export function highImpactChanges(
  data: ConfigData,
  set: Record<string, ConfigValue>,
  unset: Iterable<string> = [],
): HighImpactChange[] {
  const changes: HighImpactChange[] = [];
  for (const [path, value] of Object.entries(set)) {
    if (isHighImpactVendorPath(path) && getConfigValue(data, path) !== value) {
      changes.push({ path, label: highImpactVendorLabel(path, value) });
      continue;
    }
    if (
      isHighImpactProfileSwitchPath(path) &&
      getConfigValue(data, path) !== value
    ) {
      const role = path.split(".")[1] ?? "role";
      changes.push({
        path,
        label: `${titleWord(role)} agent profile → ${String(value)}`,
      });
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
    if (isHighImpactVendorPath(path) && path !== "agent.vendor") {
      changes.push({
        path,
        label: `Unset ${configFieldLabel(path)} (resolved vendor may change for new claims)`,
      });
      continue;
    }
    if (isHighImpactProfileSwitchPath(path)) {
      changes.push({
        path,
        label: `Unset ${configFieldLabel(path)} (role falls back to global/profile overlay)`,
      });
      continue;
    }
    if (isAgentProfileWholePath(path)) {
      const profileId = path.slice("agent.profiles.".length);
      const refs = profileReferencedByRoles(data, profileId);
      if (refs.length > 0) {
        changes.push({
          path,
          label: `Remove profile ${profileId} (referenced by ${refs.join(", ")})`,
        });
      }
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
