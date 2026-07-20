import { describe, expect, it } from "vitest";
import { ApiError, type ConfigData } from "./api";
import {
  AGENT_VENDOR_OPTIONS,
  buildConfigPatch,
  CONFIG_GROUPS,
  configFieldErrors,
  configFieldPaths,
  configSelectOptions,
  draftStagesConfigChange,
  highImpactChanges,
  profileLeafUnsetWouldEmpty,
  roleAgentPath,
} from "./configForm";

function fixture(): ConfigData {
  return {
    scheduler: {
      pollIntervalSeconds: 30,
      maxConcurrentRuns: 3,
      retryMaxAttempts: 4,
      retryBaseDelayMs: 5000,
      slowLaneWarnThresholdMs: 5000,
    },
    agent: {
      vendor: "codex",
      envKeys: ["OPENAI_API_KEY"],
      profiles: {
        fast: { vendor: "codex", model: "gpt-5-mini" },
      },
      nativeResume: { enabled: true },
      timeouts: { plannerIdleTimeoutSeconds: 300 },
    },
    tools: {
      looperPath: "/usr/local/bin/looper",
      osascriptPath: "/usr/bin/osascript",
    },
    defaults: {
      allowAutoCommit: false,
      allowAutoPush: false,
      allowAutoApprove: false,
      allowAutoMerge: false,
      allowRiskyFixes: false,
    },
    roles: {
      planner: {
        triggers: { planeAssigneeId: "planner-member" },
      },
      worker: {
        triggers: { planeAssigneeId: "worker-member" },
        agent: { profile: "fast", vendor: "claude-code", model: "haiku" },
      },
      reviewer: {
        behavior: {
          reviewEvents: { clean: "COMMENT", blocking: "COMMENT" },
          threadResolution: { enabled: true, mode: "report_only" },
        },
        autoMerge: { enabled: false },
      },
    },
    metadata: {
      configPath: "/tmp/config.toml",
      format: "toml",
      filePresent: true,
      revision: "sha256:test",
      fields: {
        "scheduler.pollIntervalSeconds": {
          source: "config-file",
          editable: false,
          applyMode: "restart",
        },
        "scheduler.maxConcurrentRuns": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "scheduler.slowLaneWarnThresholdMs": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "agent.vendor": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "agent.env": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "tools.looperPath": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "tools.osascriptPath": {
          source: "default",
          editable: true,
          applyMode: "hot",
        },
        "defaults.allowAutoPush": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
        "roles.planner.triggers.planeAssigneeId": {
          source: "config-file",
          editable: false,
          applyMode: "restart",
        },
        "roles.worker.triggers.planeAssigneeId": {
          source: "config-file",
          editable: true,
          applyMode: "hot",
        },
      },
    },
  };
}

describe("config form contract", () => {
  it("exposes only the curated scheduler fields and never secret projections", () => {
    const data = fixture();
    const scheduler = CONFIG_GROUPS.find((group) => group.id === "scheduler")!;
    const agent = CONFIG_GROUPS.find((group) => group.id === "agent")!;
    const tools = CONFIG_GROUPS.find((group) => group.id === "tools")!;
    const roles = CONFIG_GROUPS.find((group) => group.id === "roles")!;

    expect(configFieldPaths(data, scheduler)).toContain(
      "scheduler.maxConcurrentRuns",
    );
    expect(configFieldPaths(data, scheduler)).not.toContain(
      "scheduler.pollIntervalSeconds",
    );
    expect(configFieldPaths(data, agent)).toContain("agent.vendor");
    expect(configFieldPaths(data, agent)).not.toContain(
      "agent.nativeResume.enabled",
    );
    expect(configFieldPaths(data, agent)).not.toContain("agent.envKeys");
    expect(configFieldPaths(data, agent)).not.toContain(
      "agent.paramsConfigured",
    );
    expect(configFieldPaths(data, tools)).toEqual([
      "tools.looperPath",
      "tools.osascriptPath",
    ]);
    expect(configFieldPaths(data, roles)).not.toContain(
      "roles.planner.triggers.planeAssigneeId",
    );
    expect(configFieldPaths(data, roles)).toContain(
      "roles.worker.triggers.planeAssigneeId",
    );
    // Profiles are curated (add/remove UI), not free-form leaf fields.
    expect(configFieldPaths(data, agent)).not.toContain(
      "agent.profiles.fast.vendor",
    );
    expect(configFieldPaths(data, agent)).not.toContain("agent.profiles");
    // Coding-role agent bindings are always exposed as curated leaves.
    expect(configFieldPaths(data, roles)).toContain(
      roleAgentPath("worker", "profile"),
    );
    expect(configFieldPaths(data, roles)).toContain(
      roleAgentPath("planner", "vendor"),
    );
    expect(configFieldPaths(data, roles)).toContain(
      roleAgentPath("reviewer", "model"),
    );
    expect(configFieldPaths(data, roles)).toContain(
      roleAgentPath("fixer", "profile"),
    );
    expect(configFieldPaths(data, roles)).not.toContain("roles.worker.agent");
    expect(configSelectOptions("agent.vendor")).toEqual([
      ...AGENT_VENDOR_OPTIONS,
    ]);
    expect(configSelectOptions(roleAgentPath("worker", "vendor"))).toEqual([
      ...AGENT_VENDOR_OPTIONS,
    ]);
    expect(configSelectOptions("agent.profiles.fast.vendor")).toEqual([
      ...AGENT_VENDOR_OPTIONS,
    ]);
  });

  it("builds patches for agent profiles and role agent bindings without params", () => {
    const data = fixture();
    const result = buildConfigPatch(
      data,
      {
        // Leaf set on a whole-profile unset is dropped (removal wins).
        "agent.profiles.fast.model": "gpt-5",
        "agent.profiles.cheap.vendor": "opencode",
        "roles.worker.agent.profile": "cheap",
        "roles.planner.agent.model": "o3",
      },
      ["agent.profiles.fast", "roles.reviewer.agent.vendor"],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({
      "agent.profiles.cheap.vendor": "opencode",
      "roles.worker.agent.profile": "cheap",
      "roles.planner.agent.model": "o3",
    });
    expect(result.body.unset).toEqual([
      "agent.profiles.fast",
      "roles.reviewer.agent.vendor",
    ]);
    expect(JSON.stringify(result.body)).not.toMatch(/params/i);
  });

  it("builds a dirty-only typed patch and validates integer controls", () => {
    const data = fixture();
    const valid = buildConfigPatch(
      data,
      {
        "scheduler.maxConcurrentRuns": "3",
        "scheduler.slowLaneWarnThresholdMs": "8",
      },
      ["agent.env.OLD_TOKEN"],
      { "agent.env.NEW_TOKEN": "write-only-value" },
    );
    expect(valid.errors).toEqual({});
    expect(valid.body).toEqual({
      revision: "sha256:test",
      set: {
        "scheduler.slowLaneWarnThresholdMs": 8,
        "agent.env.NEW_TOKEN": "write-only-value",
      },
      unset: ["agent.env.OLD_TOKEN"],
    });

    const invalid = buildConfigPatch(
      data,
      { "scheduler.maxConcurrentRuns": "3.5" },
      [],
    );
    expect(invalid.body.set).toEqual({});
    expect(invalid.errors["scheduler.maxConcurrentRuns"]).toMatch(
      /whole number/i,
    );
  });

  it("requires confirmation for newly enabled automation and review decisions", () => {
    const changes = highImpactChanges(
      fixture(),
      {
        "agent.vendor": "opencode",
        "defaults.allowAutoCommit": true,
        "defaults.allowAutoPush": true,
        "roles.planner.autoDiscovery": true,
        "roles.reviewer.behavior.reviewEvents.clean": "APPROVE",
        "roles.reviewer.behavior.reviewEvents.blocking": "REQUEST_CHANGES",
        "roles.reviewer.behavior.threadResolution.mode": "resolve_objective",
        "roles.fixer.triggers.authorFilter": "any",
        "roles.reviewer.behavior.threadResolution.requireAuditComment": false,
      },
      [
        "agent.vendor",
        "roles.coordinator.dispatch.mode",
        "defaults.allowRiskyFixes",
        "roles.reviewer.behavior.threadResolution.mode",
        "roles.fixer.triggers.authorFilter",
        "roles.reviewer.behavior.threadResolution.requireNewHeadSinceThread",
      ],
    );
    expect(changes.map((change) => change.path)).toEqual([
      "agent.vendor",
      "defaults.allowAutoCommit",
      "defaults.allowAutoPush",
      "roles.planner.autoDiscovery",
      "roles.reviewer.behavior.reviewEvents.clean",
      "roles.reviewer.behavior.reviewEvents.blocking",
      "roles.reviewer.behavior.threadResolution.mode",
      "roles.fixer.triggers.authorFilter",
      "roles.reviewer.behavior.threadResolution.requireAuditComment",
      "agent.vendor",
      "roles.coordinator.dispatch.mode",
      "defaults.allowRiskyFixes",
      "roles.reviewer.behavior.threadResolution.mode",
      "roles.fixer.triggers.authorFilter",
      "roles.reviewer.behavior.threadResolution.requireNewHeadSinceThread",
    ]);
  });

  it("requires confirmation for role/profile vendor and profile switches", () => {
    const data = fixture();
    const changes = highImpactChanges(
      data,
      {
        "roles.worker.agent.vendor": "opencode",
        "roles.reviewer.agent.profile": "fast",
        "agent.profiles.fast.vendor": "claude-code",
      },
      [
        "roles.worker.agent.vendor",
        "roles.worker.agent.profile",
        "agent.profiles.fast.vendor",
        "agent.profiles.fast",
      ],
    );
    expect(changes.map((change) => change.path)).toEqual([
      "roles.worker.agent.vendor",
      "roles.reviewer.agent.profile",
      "agent.profiles.fast.vendor",
      "roles.worker.agent.vendor",
      "roles.worker.agent.profile",
      "agent.profiles.fast.vendor",
      "agent.profiles.fast",
    ]);
    expect(
      changes.find((change) => change.path === "agent.profiles.fast")?.label,
    ).toMatch(/referenced by worker/i);
  });

  it("treats empty profile/role model drafts as explicit vendor-default suppress", () => {
    const data = fixture();
    const result = buildConfigPatch(
      data,
      {
        "agent.profiles.fast.model": "",
        "roles.worker.agent.model": "",
      },
      [],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({
      "agent.profiles.fast.model": "",
      "roles.worker.agent.model": "",
    });
    expect(result.body.unset).toEqual([]);
  });

  it("stages blank profile/role model drafts as suppress when the leaf is absent", () => {
    const data = fixture();
    data.agent = {
      ...data.agent!,
      model: "gpt-5",
      profiles: {
        cheap: { vendor: "opencode" },
      },
    };
    data.roles!.planner = {
      ...data.roles!.planner!,
      // no agent.model leaf — inherits global
    };

    // Absent profile model + blank draft → create suppress binding.
    const profileBlank = buildConfigPatch(
      data,
      { "agent.profiles.cheap.model": "" },
      [],
    );
    expect(profileBlank.errors).toEqual({});
    expect(profileBlank.body.set).toEqual({
      "agent.profiles.cheap.model": "",
    });
    expect(profileBlank.body.unset).toEqual([]);

    // Absent role model + blank draft → create suppress binding.
    const roleBlank = buildConfigPatch(
      data,
      { "roles.planner.agent.model": "" },
      [],
    );
    expect(roleBlank.errors).toEqual({});
    expect(roleBlank.body.set).toEqual({
      "roles.planner.agent.model": "",
    });
    expect(roleBlank.body.unset).toEqual([]);

    // Already-suppress model blank draft is a no-op.
    data.roles!.planner = {
      ...data.roles!.planner!,
      agent: { model: "" },
    };
    const alreadySuppress = buildConfigPatch(
      data,
      { "roles.planner.agent.model": "" },
      [],
    );
    expect(alreadySuppress.body.set).toEqual({});
    expect(alreadySuppress.body.unset).toEqual([]);
  });

  it("treats empty role profile drafts as unset when sibling vendor/model remain", () => {
    const data = fixture();
    // Fixture worker has profile + vendor + model; clearing only profile must
    // unset the leaf so backend does not reject "" under a kept agent object.
    const result = buildConfigPatch(
      data,
      {
        "roles.worker.agent.profile": "",
      },
      [],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({});
    expect(result.body.unset).toEqual(["roles.worker.agent.profile"]);
  });

  it("draftStagesConfigChange keeps unset-only empty role and profile drafts", () => {
    const data = fixture();
    // Empty role profile/model: only unset, no set/error — onDraft must still
    // retain the draft so Save can send the unset.
    expect(
      draftStagesConfigChange(data, "roles.worker.agent.profile", ""),
    ).toBe(true);
    expect(draftStagesConfigChange(data, "roles.worker.agent.model", "")).toBe(
      true,
    );
    expect(
      draftStagesConfigChange(data, "agent.profiles.fast.model", ""),
    ).toBe(true);
    // Blank model on an absent role/profile leaf stages suppress (""), not a
    // no-op — empty is distinct from inherit for overlay resolution.
    expect(
      draftStagesConfigChange(data, "roles.planner.agent.model", ""),
    ).toBe(true);
    // Non-empty change still stages.
    expect(
      draftStagesConfigChange(data, "roles.worker.agent.profile", "other"),
    ).toBe(true);
  });

  it("promotes dual profile leaf unsets to whole-profile removal", () => {
    const data = fixture();
    const result = buildConfigPatch(
      data,
      {},
      ["agent.profiles.fast.vendor", "agent.profiles.fast.model"],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({});
    expect(result.body.unset).toEqual(["agent.profiles.fast"]);
  });

  it("promotes unsetting the only published profile leaf to whole-profile removal", () => {
    const data = fixture();
    data.agent.profiles = { cheap: { vendor: "opencode" } };
    const result = buildConfigPatch(
      data,
      {},
      ["agent.profiles.cheap.vendor"],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({});
    expect(result.body.unset).toEqual(["agent.profiles.cheap"]);
  });

  it("keeps a single profile leaf unset when the other identity remains", () => {
    const data = fixture();
    const result = buildConfigPatch(
      data,
      {},
      ["agent.profiles.fast.model"],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({});
    expect(result.body.unset).toEqual(["agent.profiles.fast.model"]);
  });

  it("preserves empty-string model suppression when unsetting profile vendor", () => {
    const data = fixture();
    data.agent.profiles = { suppress: { vendor: "codex", model: "" } };

    // Dashboard must not promote vendor unset to whole-profile removal.
    expect(
      profileLeafUnsetWouldEmpty(data, {}, [], "suppress", "vendor"),
    ).toBe(false);

    // Save must leave {model: ""} rather than unsetting agent.profiles.suppress.
    const result = buildConfigPatch(
      data,
      {},
      ["agent.profiles.suppress.vendor"],
    );
    expect(result.errors).toEqual({});
    expect(result.body.set).toEqual({});
    expect(result.body.unset).toEqual(["agent.profiles.suppress.vendor"]);
  });

  it("still promotes vendor unset when model is truly absent", () => {
    const data = fixture();
    data.agent.profiles = { cheap: { vendor: "opencode" } };
    expect(
      profileLeafUnsetWouldEmpty(data, {}, [], "cheap", "vendor"),
    ).toBe(true);
  });

  it("auto-clears retained models when staging a vendor switch", () => {
    const data = fixture();
    data.agent = {
      vendor: "codex",
      model: "gpt-5",
      envKeys: ["OPENAI_API_KEY"],
      profiles: {
        fast: { vendor: "codex", model: "gpt-5-mini" },
      },
      nativeResume: { enabled: true },
      timeouts: { plannerIdleTimeoutSeconds: 300 },
    };
    data.roles = {
      planner: {
        triggers: { planeAssigneeId: "planner-member" },
      },
      worker: {
        triggers: { planeAssigneeId: "worker-member" },
        agent: {
          profile: "fast",
          vendor: "claude-code",
          model: "haiku",
        },
      },
      reviewer: {
        behavior: {
          reviewEvents: { clean: "COMMENT", blocking: "COMMENT" },
          threadResolution: { enabled: true, mode: "report_only" },
        },
        autoMerge: { enabled: false },
        agent: { vendor: "codex" },
      },
    };

    const globalOnly = buildConfigPatch(
      data,
      { "agent.vendor": "opencode" },
      [],
    );
    expect(globalOnly.errors).toEqual({});
    expect(globalOnly.body.set).toEqual({ "agent.vendor": "opencode" });
    expect(globalOnly.body.unset).toEqual(["agent.model"]);

    const profileOnly = buildConfigPatch(
      data,
      { "agent.profiles.fast.vendor": "opencode" },
      [],
    );
    expect(profileOnly.errors).toEqual({});
    expect(profileOnly.body.set).toEqual({
      "agent.profiles.fast.vendor": "opencode",
    });
    expect(profileOnly.body.unset).toEqual(["agent.profiles.fast.model"]);

    const roleOwnedModel = buildConfigPatch(
      data,
      { "roles.worker.agent.vendor": "opencode" },
      [],
    );
    expect(roleOwnedModel.errors).toEqual({});
    expect(roleOwnedModel.body.set).toEqual({
      "roles.worker.agent.vendor": "opencode",
    });
    expect(roleOwnedModel.body.unset).toEqual(["roles.worker.agent.model"]);

    // Role vendor switch with only inherited global model: suppress via "".
    const roleInherited = buildConfigPatch(
      data,
      { "roles.reviewer.agent.vendor": "opencode" },
      [],
    );
    expect(roleInherited.errors).toEqual({});
    expect(roleInherited.body.set).toEqual({
      "roles.reviewer.agent.vendor": "opencode",
      "roles.reviewer.agent.model": "",
    });
    expect(roleInherited.body.unset).toEqual([]);

    // Profile vendor switch when profile model equals global model: unsetting
    // the profile model would re-inherit that same global model under the new
    // vendor (roles selecting the profile keep resolved model) — suppress "".
    data.agent.profiles = {
      fast: { vendor: "codex", model: "gpt-5" },
    };
    data.roles.worker = {
      triggers: { planeAssigneeId: "worker-member" },
      agent: { profile: "fast" },
    };
    const profileSameAsGlobal = buildConfigPatch(
      data,
      { "agent.profiles.fast.vendor": "opencode" },
      [],
    );
    expect(profileSameAsGlobal.errors).toEqual({});
    expect(profileSameAsGlobal.body.set).toEqual({
      "agent.profiles.fast.vendor": "opencode",
      "agent.profiles.fast.model": "",
    });
    expect(profileSameAsGlobal.body.unset).toEqual([]);

    // Model-less profile vendor switch still inherits non-empty global model
    // for roles selecting the profile — stage profile model suppress.
    data.agent.profiles = {
      bare: { vendor: "codex" },
    };
    data.roles.worker = {
      triggers: { planeAssigneeId: "worker-member" },
      agent: { profile: "bare" },
    };
    const modelLessProfile = buildConfigPatch(
      data,
      { "agent.profiles.bare.vendor": "opencode" },
      [],
    );
    expect(modelLessProfile.errors).toEqual({});
    expect(modelLessProfile.body.set).toEqual({
      "agent.profiles.bare.vendor": "opencode",
      "agent.profiles.bare.model": "",
    });
    expect(modelLessProfile.body.unset).toEqual([]);

    // Global vendor switch: role owns model while inheriting global vendor.
    data.agent.profiles = {
      fast: { vendor: "codex", model: "gpt-5-mini" },
    };
    data.roles.worker = {
      triggers: { planeAssigneeId: "worker-member" },
      agent: { model: "gpt-5" },
    };
    data.roles.reviewer = {
      behavior: {
        reviewEvents: { clean: "COMMENT", blocking: "COMMENT" },
        threadResolution: { enabled: true, mode: "report_only" },
      },
      autoMerge: { enabled: false },
    };
    const globalWithRoleModel = buildConfigPatch(
      data,
      { "agent.vendor": "claude-code" },
      [],
    );
    expect(globalWithRoleModel.errors).toEqual({});
    expect(globalWithRoleModel.body.set).toEqual({
      "agent.vendor": "claude-code",
    });
    expect(globalWithRoleModel.body.unset.sort()).toEqual(
      ["agent.model", "roles.worker.agent.model"].sort(),
    );

    // Global vendor switch: role selects a profile that inherits global vendor
    // but owns a non-empty model — clear the profile model companion.
    data.agent.profiles = {
      shared: { model: "gpt-5-mini" },
    };
    data.roles.worker = {
      triggers: { planeAssigneeId: "worker-member" },
      agent: { profile: "shared" },
    };
    const globalWithProfileModel = buildConfigPatch(
      data,
      { "agent.vendor": "claude-code" },
      [],
    );
    expect(globalWithProfileModel.errors).toEqual({});
    expect(globalWithProfileModel.body.set).toEqual({
      "agent.vendor": "claude-code",
      "agent.profiles.shared.model": "",
    });
    expect(globalWithProfileModel.body.unset).toEqual(["agent.model"]);

    // Global vendor switch leaves role/profile models alone when the role
    // overrides vendor (resolved vendor does not inherit global).
    data.roles.worker = {
      triggers: { planeAssigneeId: "worker-member" },
      agent: { vendor: "codex", model: "gpt-5" },
    };
    const globalWithRoleVendor = buildConfigPatch(
      data,
      { "agent.vendor": "claude-code" },
      [],
    );
    expect(globalWithRoleVendor.errors).toEqual({});
    expect(globalWithRoleVendor.body.set).toEqual({
      "agent.vendor": "claude-code",
    });
    expect(globalWithRoleVendor.body.unset).toEqual(["agent.model"]);
  });

  it("confirms automatic commit only when the change can enable it", () => {
    const disabled = fixture();
    expect(
      highImpactChanges(disabled, { "defaults.allowAutoCommit": true }),
    ).toEqual([
      {
        path: "defaults.allowAutoCommit",
        label: "Allow Auto Commit",
      },
    ]);

    const enabled = fixture();
    enabled.defaults!.allowAutoCommit = true;
    expect(
      highImpactChanges(enabled, { "defaults.allowAutoCommit": false }),
    ).toEqual([]);
  });

  it("maps backend validation details to dotted fields", () => {
    const error = new ApiError("invalid config", {
      status: 400,
      details: {
        issues: [
          {
            path: "scheduler.maxConcurrentRuns",
            message: "must be greater than zero",
          },
        ],
      },
    });
    expect(configFieldErrors(error)).toEqual({
      "scheduler.maxConcurrentRuns": "must be greater than zero",
    });
  });
});
