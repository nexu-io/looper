import { describe, expect, it } from "vitest";
import { ApiError, type ConfigData } from "./api";
import {
  buildConfigPatch,
  CONFIG_GROUPS,
  configFieldErrors,
  configFieldPaths,
  highImpactChanges,
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
