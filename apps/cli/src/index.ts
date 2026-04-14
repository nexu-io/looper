#!/usr/bin/env bun

import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { basename, join, resolve, sep } from "node:path";
import { cac } from "cac";

import {
  type ApiClient,
  CliApiError,
  type FetchLike,
  createApiClient,
} from "./client";
import { printJson, printSection, printTable } from "./format";

type Writer = (line: string) => void;

interface CliDeps {
  fetchImpl?: FetchLike;
  loadConfigImpl?: (options: {
    argv: string[];
    env: Record<string, string | undefined>;
    cwd: string;
  }) => Promise<LoadedCliConfig>;
  readFileImpl?: (path: string, encoding: "utf8") => Promise<string>;
  stdout?: Writer;
  stderr?: Writer;
  env?: Record<string, string | undefined>;
  cwd?: string;
  isStdoutTty?: boolean;
  launchShellImpl?: (options: {
    cwd: string;
    env: Record<string, string | undefined>;
  }) => Promise<number>;
}

interface CliContext {
  args: ParsedArgs;
  write: Writer;
  writeError: Writer;
  config: LoadedCliConfig;
  client: ApiClient;
  readFileImpl: (path: string, encoding: "utf8") => Promise<string>;
  showHelp: (commandName?: string) => void;
  env: Record<string, string | undefined>;
  cwd: string;
  isStdoutTty: boolean;
  launchShell: (cwd: string) => Promise<number>;
}

type CliRuntime = Omit<CliContext, "args">;

interface LoadedCliConfig {
  config: {
    server: {
      host: string;
      port: number;
      baseUrl?: string;
      localToken?: string;
    };
    daemon: {
      mode: string;
      logDir: string;
    };
  };
  metadata: {
    configPath: string;
  };
}

interface ParsedArgs {
  positionals: string[];
  flags: Map<string, string[]>;
}

interface PullRequestRef {
  repo: string;
  prNumber: number;
}

interface ProjectSummary {
  id: string;
  repoPath: string;
  repo: string | null;
}

interface ActiveRunItem {
  seq: number;
  runId: string | null;
  loopId: string;
  projectId: string;
  type: string;
  status: string;
  currentStep: string | null;
  startedAt: string | null;
  target:
    | {
        type: "project";
        projectId: string;
        label: string;
      }
    | {
        type: "issue";
        repo: string;
        issueNumber: number;
        label: string;
      }
    | {
        type: "pull_request";
        repo: string;
        prNumber: number;
        label: string;
      };
  agent: {
    active: true;
    activeCount: number;
    executionId: string;
    vendor: string;
    pid: number | null;
    startedAt: string;
    lastHeartbeatAt: string | null;
    heartbeatCount: number;
    status: string;
  } | null;
  worktree: {
    id: string | null;
    path: string;
    branch: string | null;
  } | null;
}

interface LoopLogsEnvelope {
  seq: number;
  loopId: string;
  loopType: string;
  loopStatus: string;
  run: {
    runId: string;
    status: string;
    currentStep: string | null;
    startedAt: string;
    endedAt: string | null;
    summary: string | null;
    errorMessage: string | null;
  } | null;
  agent: {
    executionId: string;
    vendor: string;
    status: string;
    pid: number | null;
    startedAt: string;
    endedAt: string | null;
    heartbeatCount: number;
    lastHeartbeatAt: string | null;
    summary: string | null;
    parseStatus: string | null;
    stdout: string;
    stderr: string;
  } | null;
}

const CONFIG_FLAGS = new Set([
  "config",
  "host",
  "port",
  "db-path",
  "log-dir",
  "daemon-mode",
  "bun-path",
  "git-path",
  "gh-path",
  "osascript-path",
]);

export async function runCli(
  argv: string[],
  deps: CliDeps = {},
): Promise<number> {
  const env = deps.env ?? (process.env as Record<string, string | undefined>);
  const cwd = deps.cwd ?? process.cwd();
  const write = deps.stdout ?? ((line) => console.log(line));
  const writeError = deps.stderr ?? ((line) => console.error(line));

  try {
    const loadConfigImpl = deps.loadConfigImpl ?? loadCliConfig;
    const config = await loadConfigImpl({
      argv: extractConfigArgs(argv),
      env,
      cwd,
    });
    const client = createApiClient({
      baseUrl:
        config.config.server.baseUrl ??
        `http://${config.config.server.host}:${config.config.server.port}`,
      token: env.LOOPER_TOKEN ?? config.config.server.localToken,
      fetchImpl: deps.fetchImpl,
    });
    const runtime: CliRuntime = {
      write,
      writeError,
      config,
      client,
      readFileImpl: deps.readFileImpl ?? readFile,
      showHelp: () => {},
      env,
      cwd,
      isStdoutTty: deps.isStdoutTty ?? Boolean(process.stdout.isTTY),
      launchShell: async (shellCwd) =>
        (deps.launchShellImpl ?? launchInteractiveShell)({
          cwd: shellCwd,
          env,
        }),
    };

    const cli = createCli(runtime);
    runtime.showHelp = (commandName) => {
      outputCommandHelp(cli, commandName);
    };
    cli.parse(["bun", "looper", ...argv], { run: false });

    if (!cli.matchedCommand) {
      if (argv.includes("--help") || argv.includes("-h")) {
        return 0;
      }

      outputCommandHelp(cli, argv[0]);
      return 0;
    }

    await cli.runMatchedCommand();
    return 0;
  } catch (error) {
    writeError(formatError(error));
    return 1;
  }
}

async function dispatch(context: CliContext): Promise<void> {
  const [command, subcommand] = context.args.positionals;

  switch (command) {
    case "status":
      return runStatus(context);
    case "project":
      if (subcommand === "list") {
        return runProjectList(context);
      }
      if (subcommand === "add") {
        return runProjectAdd(context);
      }
      context.showHelp("project");
      return;
    case "config":
      if (subcommand !== "show") {
        context.showHelp("config");
        return;
      }
      return runConfigShow(context);
    case "daemon":
      if (subcommand === "status") {
        return runDaemonStatus(context);
      }
      if (subcommand === "logs") {
        return runDaemonLogs(context);
      }
      context.showHelp("daemon");
      return;
    case "loop":
      if (subcommand === "list") {
        return runLoopList(context);
      }
      if (subcommand === "start") {
        return runLoopStart(context);
      }
      if (subcommand === "pause") {
        return runLoopPause(context);
      }
      context.showHelp("loop");
      return;
    case "work":
      return runWorkCreate(context);
    case "plan":
      return runPlannerCreate(context);
    case "task":
      throw new Error("task commands were removed; use looper work instead");
    case "worker":
      throw new Error("worker commands were removed; use looper work instead");
    case "workers":
      throw new Error("worker commands were removed; use looper work instead");
    case "pr":
      if (subcommand === "list") {
        return runPrList(context);
      }
      if (subcommand === "show") {
        return runPrShow(context);
      }
      if (subcommand === "status") {
        return runPrStatus(context);
      }
      context.showHelp("pr");
      return;
    case "review":
      return runReviewCreate(context);
    case "run":
      if (subcommand === "list") {
        return runRunList(context);
      }
      context.showHelp("run");
      return;
    case "ps":
      return runPs(context);
    case "jump":
      return runJump(context);
    case "logs":
      return runLogs(context);
    case "stop":
      return runStop(context);
    default:
      break;
  }

  throw new Error(`Unknown command: ${context.args.positionals.join(" ")}`);
}

function createCli(runtime: CliRuntime) {
  const cli = cac("looper");

  addGlobalOptions(cli);

  cli.command("status", "Show service status").action(async (options) => {
    await dispatch(createContext(runtime, ["status"], options));
  });

  cli
    .command("project [...args]", "Project commands")
    .usage("project <subcommand> [options]")
    .option("--repo-path <path>", "Repository path")
    .option("--id <id>", "Project id")
    .option("--name <name>", "Project name")
    .option("--base-branch <branch>", "Base branch")
    .option("--worktree-root <path>", "Worktree root")
    .option("--repo <repo>", "Repository slug")
    .example((name) => `  $ ${name} project list`)
    .example((name) => `  $ ${name} project add /path/to/repo`)
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["project", ...args], options));
    });

  cli
    .command("config [...args]", "Config commands")
    .usage("config <subcommand> [options]")
    .example((name) => `  $ ${name} config show --json`)
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["config", ...args], options));
    });

  cli
    .command("daemon [...args]", "Daemon commands")
    .usage("daemon <subcommand> [options]")
    .option("--lines <count>", "Line count")
    .example((name) => `  $ ${name} daemon status`)
    .example((name) => `  $ ${name} daemon logs --lines 50`)
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["daemon", ...args], options));
    });

  cli
    .command("loop [...args]", "Loop commands")
    .usage("loop <subcommand> [options]")
    .option("--id <id>", "Loop id")
    .option("--type <type>", "Loop type")
    .option("--pr <repo#number>", "Pull request reference")
    .example((name) => `  $ ${name} loop list`)
    .example(
      (name) => `  $ ${name} loop start --type reviewer --pr acme/looper#42`,
    )
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["loop", ...args], options));
    });

  cli
    .command("work", "Create a worker run")
    .option("--project <projectId>", "Project id")
    .option("--title <title>", "Task title")
    .option("--prompt <text>", "Implementation prompt")
    .option("--issue <number>", "Issue number")
    .option("--spec <path>", "Spec path")
    .option("--repo <repo>", "Repository slug")
    .option("--base-branch <branch>", "Base branch")
    .example(
      (name) =>
        `  $ ${name} work --project project_1 --title "Ship CLI" --spec specs/ship-cli.md`,
    )
    .example((name) => `  $ ${name} work --project project_1 --issue 123`)
    .action(async (options) => {
      await dispatch(createContext(runtime, ["work"], options));
    });

  cli
    .command("plan", "Create a planner run")
    .option("--project <projectId>", "Project id")
    .option("--issue <number>", "Issue number")
    .example((name) => `  $ ${name} plan --project project_1 --issue 123`)
    .action(async (options) => {
      await dispatch(createContext(runtime, ["plan"], options));
    });

  cli
    .command("pr [...args]", "Pull request commands")
    .usage("pr <subcommand> [options]")
    .example((name) => `  $ ${name} pr list`)
    .example((name) => `  $ ${name} pr show acme/looper#42`)
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["pr", ...args], options));
    });

  cli
    .command("review <pr>", "Create a reviewer task for a pull request")
    .option("--project <projectId>", "Project id")
    .option("--loop", "Keep reviewing when new commits are pushed")
    .example((name) => `  $ ${name} review 123`)
    .example((name) => `  $ ${name} review acme/looper#42 --loop`)
    .action(async (pr, options) => {
      await dispatch(createContext(runtime, ["review", pr], options));
    });

  cli
    .command("ps", "Show running loops")
    .option("--type <type>", "Filter by loop type")
    .option("--project <projectId>", "Filter by project id")
    .example((name) => `  $ ${name} ps`)
    .example((name) => `  $ ${name} ps --type reviewer --project project_1`)
    .action(async (options) => {
      await dispatch(createContext(runtime, ["ps"], options));
    });

  cli
    .command("jump [id]", "Print shell command for a loop worktree")
    .option("--print-path", "Print the worktree path only")
    .option("--shell-integration <shell>", "Print shell integration helper")
    .example((name) => `  $ eval \"$(${name} jump 12)\"`)
    .example((name) => `  $ ${name} jump 12 --print-path`)
    .example((name) => `  $ ${name} jump --shell-integration bash`)
    .action(async (id, options) => {
      await dispatch(createContext(runtime, ["jump", id], options));
    });

  cli
    .command("logs <id>", "Show logs for a loop")
    .option("--stderr", "Show stderr instead of stdout")
    .option("--tail <count>", "Show the last N lines")
    .option("--full", "Show the full output")
    .example((name) => `  $ ${name} logs 12`)
    .example((name) => `  $ ${name} logs 12 --stderr --tail 50`)
    .action(async (id, options) => {
      await dispatch(createContext(runtime, ["logs", id], options));
    });

  cli
    .command("stop <id>", "Stop an active loop")
    .example((name) => `  $ ${name} stop 12`)
    .action(async (id, options) => {
      await dispatch(createContext(runtime, ["stop", id], options));
    });

  cli
    .command("run [...args]", "Run commands")
    .usage("run <subcommand> [options]")
    .option("--loop <loopId>", "Filter by loop id")
    .example((name) => `  $ ${name} run list`)
    .example((name) => `  $ ${name} run list --loop loop_1`)
    .action(async (args, options) => {
      await dispatch(createContext(runtime, ["run", ...args], options));
    });

  cli.help();

  return cli;
}

function addGlobalOptions(cli: ReturnType<typeof cac>) {
  cli.option("--json", "Emit JSON output");
  cli.option("--config <path>", "Config path");
  cli.option("--host <host>", "Server host");
  cli.option("--port <port>", "Server port");
  cli.option("--db-path <path>", "Database path");
  cli.option("--log-dir <path>", "Daemon log directory");
  cli.option("--daemon-mode <mode>", "Daemon mode");
  cli.option("--bun-path <path>", "Bun binary path");
  cli.option("--git-path <path>", "Git binary path");
  cli.option("--gh-path <path>", "GitHub CLI path");
  cli.option("--osascript-path <path>", "osascript binary path");
}

function outputCommandHelp(
  cli: ReturnType<typeof cac>,
  commandName?: string,
): void {
  if (!commandName) {
    cli.outputHelp();
    return;
  }

  const command = cli.commands.find(
    (entry) =>
      entry.name === commandName || entry.aliasNames.includes(commandName),
  );
  if (command) {
    command.outputHelp();
    return;
  }

  cli.outputHelp();
}

function createContext(
  runtime: CliRuntime,
  positionals: string[],
  options?: Record<string, unknown>,
): CliContext {
  return {
    ...runtime,
    args: buildParsedArgs(positionals, options),
  };
}

function buildParsedArgs(
  positionals: string[],
  options?: Record<string, unknown>,
): ParsedArgs {
  const flags = new Map<string, string[]>();

  for (const [name, value] of Object.entries(options ?? {})) {
    if (value === undefined || value === false || name === "--") {
      continue;
    }

    const key = camelToKebab(name);
    if (Array.isArray(value)) {
      flags.set(
        key,
        value.map((item) => String(item)),
      );
      continue;
    }

    flags.set(key, [value === true ? "true" : String(value)]);
  }

  return { positionals, flags };
}

function camelToKebab(value: string): string {
  return value.replace(/([a-z0-9])([A-Z])/g, "$1-$2").toLowerCase();
}

async function runStatus(context: CliContext) {
  const data =
    await context.client.get<Record<string, unknown>>("/api/v1/status");
  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  const status = data as {
    service: Record<string, unknown>;
    storage: Record<string, unknown>;
    scheduler: Record<string, unknown>;
    loops: Record<string, Record<string, unknown>>;
    notifications: Record<string, unknown>;
    tools: Record<string, unknown>;
  };
  printSection(context.write, "Service", [
    ["healthy", status.service.healthy as boolean],
    ["version", status.service.version as string],
    ["daemonMode", status.service.daemonMode as string],
    ["startedAt", status.service.startedAt as string],
  ]);
  context.write("");
  printSection(context.write, "Storage", [
    ["dbPath", status.storage.dbPath as string],
    ["schemaVersion", status.storage.schemaVersion as string],
    ["healthy", status.storage.healthy as boolean],
    [
      "pendingMigrations",
      Array.isArray(status.storage.pendingMigrations)
        ? status.storage.pendingMigrations.join(", ") || "none"
        : "none",
    ],
  ]);
  context.write("");
  printSection(context.write, "Scheduler", [
    ["healthy", status.scheduler.healthy as boolean],
    ["queuedItems", status.scheduler.queuedItems as number],
    ["runningItems", status.scheduler.runningItems as number],
  ]);
  context.write("");
  printTable(
    context.write,
    Object.entries(status.loops).map(([type, summary]) => ({
      type,
      ...summary,
    })),
  );
  context.write("");
  printSection(
    context.write,
    "Tools",
    Object.entries(status.tools) as Array<[string, boolean]>,
  );
  context.write("");
  printSection(
    context.write,
    "Notifications",
    Object.entries(status.notifications) as Array<[string, boolean]>,
  );
}

async function runConfigShow(context: CliContext) {
  const data =
    await context.client.get<Record<string, unknown>>("/api/v1/config");
  printJson(context.write, data);
}

async function runProjectList(context: CliContext) {
  const data = await context.client.get<{
    items: Array<Record<string, unknown>>;
  }>("/api/v1/projects");

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printTable(
    context.write,
    data.items.map((project) => ({
      id: project.id as string,
      name: project.name as string,
      repoPath: project.repoPath as string,
      baseBranch: (project.baseBranch as string | null | undefined) ?? "-",
      repo: (project.repo as string | null | undefined) ?? "-",
      updatedAt: project.updatedAt as string,
    })),
  );
}

async function runProjectAdd(context: CliContext) {
  const repoPathArg =
    context.args.positionals[2] ?? getFlag(context.args, "repo-path");
  if (!repoPathArg) {
    context.showHelp("project");
    return;
  }

  const repoPath = resolve(repoPathArg);
  const id = getFlag(context.args, "id") ?? deriveProjectId(repoPath);
  const name = getFlag(context.args, "name") ?? id;
  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/projects",
    {
      id,
      name,
      repoPath,
      baseBranch: getFlag(context.args, "base-branch"),
      worktreeRoot: getFlag(context.args, "worktree-root"),
      repo: getFlag(context.args, "repo"),
    },
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Project added", [
    ["id", data.id as string],
    ["name", data.name as string],
    ["repoPath", data.repoPath as string],
    ["baseBranch", (data.baseBranch as string | null | undefined) ?? "-"],
    ["repo", (data.repo as string | null | undefined) ?? "-"],
    ["discoveredPullRequests", data.discoveredPullRequests as number],
    ["discoveredWorktrees", data.discoveredWorktrees as number],
  ]);

  const warnings = (data.warnings as string[] | undefined) ?? [];
  if (warnings.length > 0) {
    context.write("");
    printSection(
      context.write,
      "Warnings",
      warnings.map((warning, index) => [String(index + 1), warning]),
    );
  }
}

async function runDaemonStatus(context: CliContext) {
  let health: unknown = null;
  let reachable = false;

  try {
    health =
      await context.client.get<Record<string, unknown>>("/api/v1/healthz");
    reachable = true;
  } catch (error) {
    if (!(error instanceof CliApiError)) {
      throw error;
    }
  }

  const data = {
    mode: context.config.config.daemon.mode,
    configPath: context.config.metadata.configPath,
    logDir: context.config.config.daemon.logDir,
    apiReachable: reachable,
    health,
  };

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Daemon", [
    ["mode", data.mode],
    ["configPath", data.configPath],
    ["logDir", data.logDir],
    ["apiReachable", data.apiReachable],
  ]);
  if (reachable) {
    context.write("");
    printJson(context.write, health);
  }
}

async function runDaemonLogs(context: CliContext) {
  const lineCount = Number(getFlag(context.args, "lines") ?? "50");
  if (!Number.isInteger(lineCount) || lineCount <= 0) {
    throw new Error("--lines must be a positive integer");
  }

  const logPath = join(context.config.config.daemon.logDir, "looperd.log");
  const content = await context.readFileImpl(logPath, "utf8");
  const lines = content.trimEnd().split("\n");
  const selected = lines.slice(-lineCount);

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, { logPath, lines: selected });
  }

  context.write(logPath);
  for (const line of selected) {
    context.write(line);
  }
}

async function runLoopList(context: CliContext) {
  const data = await context.client.get<{
    items: Array<Record<string, unknown>>;
  }>("/api/v1/loops");
  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printTable(
    context.write,
    data.items.map((loop) => ({
      id: loop.id as string,
      type: loop.type as string,
      status: loop.status as string,
      target:
        loop.targetType === "project"
          ? String(loop.targetId ?? "-")
          : loop.targetType === "issue"
            ? String(loop.targetId ?? "-")
            : `${loop.repo}#${loop.prNumber}`,
      projectId: loop.projectId as string,
    })),
  );
}

async function runLoopStart(context: CliContext) {
  const existingLoopId =
    context.args.positionals[2] ?? getFlag(context.args, "id");
  const data = existingLoopId
    ? await context.client.post<Record<string, unknown>>(
        `/api/v1/loops/${encodeURIComponent(existingLoopId)}/start`,
      )
    : await context.client.post<Record<string, unknown>>(
        "/api/v1/loops",
        await buildLoopCreateBody(context),
      );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Loop started", [
    ["id", data.id as string],
    ["type", data.type as string],
    ["status", data.status as string],
  ]);
}

async function runReviewCreate(context: CliContext) {
  const refText = context.args.positionals[1];
  if (!refText) {
    throw new Error("Usage: looper review <pr> [--loop]");
  }

  const target = await resolveReviewTarget(context, refText);
  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/loops",
    {
      projectId: target.projectId,
      type: "reviewer",
      targetType: "pull_request",
      repo: target.repo,
      prNumber: target.prNumber,
      status: "running",
      metadata: {
        followUpdates: hasFlag(context.args, "loop"),
        manual: true,
      },
    },
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Reviewer started", [
    ["id", data.id as string],
    ["projectId", data.projectId as string],
    ["pr", `${data.repo}#${data.prNumber}`],
    ["status", data.status as string],
    ["loop", String(hasFlag(context.args, "loop"))],
  ]);
}

async function buildLoopCreateBody(context: CliContext) {
  const type = requireFlag(context.args, "type");
  const pr = getFlag(context.args, "pr");

  if (pr) {
    const ref = parsePullRequestRef(pr);
    const snapshot = await context.client.get<Record<string, unknown>>(
      `/api/v1/pull-requests/${encodeURIComponent(ref.repo)}/${ref.prNumber}`,
    );
    return {
      projectId: snapshot.projectId,
      type,
      targetType: "pull_request",
      repo: ref.repo,
      prNumber: ref.prNumber,
      status: "running",
    };
  }

  throw new Error("loop start requires --pr <repo>#<number>");
}

async function runLoopPause(context: CliContext) {
  const loopId = context.args.positionals[2] ?? getFlag(context.args, "id");
  if (!loopId) {
    throw new Error("Usage: looper loop pause <id>");
  }

  const data = await context.client.post<Record<string, unknown>>(
    `/api/v1/loops/${encodeURIComponent(loopId)}/pause`,
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Loop paused", [
    ["id", data.id as string],
    ["status", data.status as string],
  ]);
}

async function runWorkCreate(context: CliContext) {
  const issueNumberValue = getFlag(context.args, "issue");
  let issueNumber: number | undefined;
  if (issueNumberValue != null) {
    if (
      getFlag(context.args, "prompt") != null ||
      getFlag(context.args, "spec") != null
    ) {
      throw new Error("--issue cannot be combined with --prompt or --spec");
    }

    const parsedIssueNumber = Number(issueNumberValue);
    if (!Number.isInteger(parsedIssueNumber) || parsedIssueNumber <= 0) {
      throw new Error("--issue must be a positive integer");
    }
    issueNumber = parsedIssueNumber;
  }

  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/workers",
    {
      projectId: await resolveCommandProjectId(context),
      ...(issueNumber == null
        ? { title: requireFlag(context.args, "title") }
        : hasFlag(context.args, "title")
          ? { title: requireFlag(context.args, "title") }
          : {}),
      ...(getFlag(context.args, "prompt") != null
        ? { prompt: getFlag(context.args, "prompt") }
        : {}),
      ...(issueNumber == null ? {} : { issueNumber }),
      ...(getFlag(context.args, "spec") != null
        ? { specPath: getFlag(context.args, "spec") }
        : {}),
      repo: getFlag(context.args, "repo"),
      baseBranch: getFlag(context.args, "base-branch"),
    },
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Worker started", [
    ["id", data.id as string],
    ["title", data.title as string],
    ["status", data.status as string],
  ]);
}

async function runPlannerCreate(context: CliContext) {
  const issueNumber = Number(requireFlag(context.args, "issue"));
  if (!Number.isInteger(issueNumber) || issueNumber <= 0) {
    throw new Error("--issue must be a positive integer");
  }

  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/planners",
    {
      projectId: await resolveCommandProjectId(context),
      issueNumber,
    },
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Planner started", [
    ["id", data.id as string],
    ["issueNumber", data.issueNumber as number],
    ["status", data.status as string],
  ]);
}

async function runPrList(context: CliContext) {
  const data = await context.client.get<{
    items: Array<Record<string, unknown>>;
  }>("/api/v1/pull-requests");
  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printTable(
    context.write,
    data.items.map((item) => ({
      pr: `${item.repo}#${item.prNumber}`,
      title: item.title as string,
      reviewState: item.reviewState as string,
      checks: item.checksSummary as string,
      reviewer: (item.reviewer as string | null | undefined) ?? "-",
      fixer: (item.fixer as string | null | undefined) ?? "-",
    })),
  );
}

async function runPrShow(context: CliContext) {
  const refText = context.args.positionals[2];
  if (!refText) {
    throw new Error("Usage: looper pr show <repo>#<number>");
  }
  const ref = parsePullRequestRef(refText);
  const data = await context.client.get<Record<string, unknown>>(
    `/api/v1/pull-requests/${encodeURIComponent(ref.repo)}/${ref.prNumber}`,
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Pull request", [
    ["repo", data.repo as string],
    ["prNumber", data.prNumber as number],
    ["title", data.title as string],
    ["reviewState", data.reviewState as string],
    ["checksSummary", data.checksSummary as string],
  ]);
}

async function runPrStatus(context: CliContext) {
  const refText = context.args.positionals[2];
  if (!refText) {
    throw new Error("Usage: looper pr status <repo>#<number>");
  }
  const ref = parsePullRequestRef(refText);
  const data = await context.client.get<Record<string, unknown>>(
    `/api/v1/pull-requests/${encodeURIComponent(ref.repo)}/${ref.prNumber}/status`,
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printSection(context.write, "Pull request status", [
    ["pr", `${data.repo}#${data.prNumber}`],
    ["reviewState", data.reviewState as string],
    ["checksSummary", data.checksSummary as string],
    ["unresolvedThreads", data.unresolvedThreadCount as number],
    [
      "latestRunStatus",
      (data.loopStatus as { latestRunStatus?: string } | undefined)
        ?.latestRunStatus ?? "-",
    ],
  ]);
}

async function runRunList(context: CliContext) {
  const loopId = getFlag(context.args, "loop");
  const path = loopId
    ? `/api/v1/runs?loopId=${encodeURIComponent(loopId)}`
    : "/api/v1/runs";
  const data = await context.client.get<{
    items: Array<Record<string, unknown>>;
  }>(path);

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  printTable(
    context.write,
    data.items.map((run) => ({
      id: run.id as string,
      loopId: run.loopId as string,
      status: run.status as string,
      currentStep: (run.currentStep as string | null | undefined) ?? "-",
      startedAt: run.startedAt as string,
    })),
  );
}

async function runPs(context: CliContext) {
  const searchParams = new URLSearchParams();
  const type = getFlag(context.args, "type");
  const projectId = getFlag(context.args, "project");

  if (type) {
    searchParams.set("type", type);
  }
  if (projectId) {
    searchParams.set("projectId", projectId);
  }

  const query = searchParams.toString();
  const path = query ? `/api/v1/runs/active?${query}` : "/api/v1/runs/active";
  const data = await context.client.get<{ items: ActiveRunItem[] }>(path);

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  if (data.items.length === 0) {
    context.write("No running or queued loops.");
    return;
  }

  printTable(
    context.write,
    data.items.map((item) => ({
      "#": item.seq,
      type: item.type,
      target: item.target.label,
      step: item.currentStep ?? "-",
      agent: item.agent?.vendor ?? "-",
      pid: item.agent?.pid ?? "-",
      status: item.status,
      age: formatRelativeAge(item.startedAt),
    })),
  );
}

async function runJump(context: CliContext) {
  const shell = getFlag(context.args, "shell-integration");
  if (shell) {
    context.write(buildShellIntegration(shell));
    return;
  }

  const selector = requireSelector(context, "Usage: looper jump <id>");
  const data = await context.client.get<ActiveRunItem>(
    `/api/v1/runs/active/${encodeURIComponent(selector)}`,
  );

  if (!data.worktree?.path) {
    throw new Error(`Loop ${selector} has no active worktree path`);
  }

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, {
      seq: data.seq,
      loopId: data.loopId,
      projectId: data.projectId,
      worktree: data.worktree,
    });
  }

  if (hasFlag(context.args, "print-path")) {
    context.write(data.worktree.path);
    return;
  }

  if (context.isStdoutTty) {
    await context.launchShell(data.worktree.path);
    return;
  }

  context.write(`cd -- ${quoteShellArg(data.worktree.path)}`);
}

async function launchInteractiveShell(options: {
  cwd: string;
  env: Record<string, string | undefined>;
}): Promise<number> {
  const shell = options.env.SHELL || "/bin/zsh";
  const subprocess = Bun.spawn([shell, "-i"], {
    cwd: options.cwd,
    env: options.env,
    stdin: "inherit",
    stdout: "inherit",
    stderr: "inherit",
  });
  return await subprocess.exited;
}

async function runLogs(context: CliContext) {
  const selector = requireSelector(context, "Usage: looper logs <id>");
  const data = await context.client.get<LoopLogsEnvelope>(
    `/api/v1/loops/${encodeURIComponent(selector)}/logs`,
  );

  if (hasFlag(context.args, "json")) {
    return printJson(context.write, data);
  }

  const stream = hasFlag(context.args, "stderr")
    ? (data.agent?.stderr ?? "")
    : (data.agent?.stdout ?? "");
  const tailCount = hasFlag(context.args, "full")
    ? undefined
    : (readOptionalPositiveIntegerFlag(context.args, "tail") ?? 100);

  context.write(`Loop #${data.seq} · ${data.loopType} · ${data.loopStatus}`);
  if (data.run) {
    context.write(
      `Run ${data.run.runId} · step: ${data.run.currentStep ?? "-"}`,
    );
  } else {
    context.write("Run - · step: -");
  }

  if (!data.agent) {
    context.write("No agent output for the current step.");
    return;
  }

  context.write(
    `Agent: ${data.agent.vendor} · pid ${data.agent.pid ?? "-"} · ${data.agent.status}`,
  );
  context.write("");

  const content =
    tailCount === undefined ? stream : tailText(stream, tailCount);
  if (!content) {
    context.write("No output captured.");
    return;
  }

  for (const line of content.split("\n")) {
    context.write(line);
  }
}

async function runStop(context: CliContext) {
  const selector = requireSelector(context, "Usage: looper stop <id>");
  const data = await context.client.post<Record<string, unknown>>(
    `/api/v1/runs/active/${encodeURIComponent(selector)}/stop`,
  );
  const stopped = Boolean(data.stopped);

  if (hasFlag(context.args, "json")) {
    printJson(context.write, data);
    if (!stopped) {
      throw new Error(`Loop ${selector} could not be stopped`);
    }
    return;
  }

  printSection(context.write, "Loop stopped", [
    ["loopId", data.loopId as string],
    ["runId", (data.runId as string | undefined) ?? "-"],
    ["executionId", (data.executionId as string | undefined) ?? "-"],
    ["vendor", (data.vendor as string | undefined) ?? "-"],
    ["pid", (data.pid as number | null | undefined) ?? "-"],
    ["stopped", stopped],
  ]);

  if (!stopped) {
    throw new Error(`Loop ${selector} could not be stopped`);
  }
}

function formatRelativeAge(startedAt: string | null): string {
  if (!startedAt) {
    return "-";
  }

  const started = Date.parse(startedAt);
  if (Number.isNaN(started)) {
    return "-";
  }

  const diffMs = Math.max(Date.now() - started, 0);
  const totalMinutes = Math.floor(diffMs / 60_000);
  if (totalMinutes < 1) {
    return "<1m";
  }
  if (totalMinutes < 60) {
    return `${totalMinutes}m`;
  }

  const totalHours = Math.floor(totalMinutes / 60);
  if (totalHours < 24) {
    const remainingMinutes = totalMinutes % 60;
    return remainingMinutes === 0
      ? `${totalHours}h`
      : `${totalHours}h${remainingMinutes}m`;
  }

  const totalDays = Math.floor(totalHours / 24);
  const remainingHours = totalHours % 24;
  return remainingHours === 0
    ? `${totalDays}d`
    : `${totalDays}d${remainingHours}h`;
}

function requireSelector(context: CliContext, usage: string): string {
  const selector = context.args.positionals[1];
  if (!selector) {
    throw new Error(usage);
  }

  return selector;
}

function readOptionalPositiveIntegerFlag(
  args: ParsedArgs,
  name: string,
): number | undefined {
  const value = getFlag(args, name);
  if (!value) {
    return undefined;
  }

  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new Error(`--${name} must be a positive integer`);
  }

  return parsed;
}

function tailText(value: string, count: number): string {
  const lines = value.split("\n");
  const trimmed = lines.at(-1) === "" ? lines.slice(0, -1) : lines;
  return trimmed.slice(-count).join("\n");
}

function quoteShellArg(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

function buildShellIntegration(shell: string): string {
  switch (shell) {
    case "bash":
    case "zsh":
      return 'lj() { eval "$(looper jump "$@")"; }';
    case "fish":
      return "function lj\n  eval (looper jump $argv)\nend";
    default:
      throw new Error(`Unsupported shell: ${shell}`);
  }
}

function extractConfigArgs(argv: string[]): string[] {
  const extracted: string[] = [];

  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (!arg) {
      continue;
    }
    if (!arg.startsWith("--")) {
      continue;
    }

    const trimmed = arg.slice(2);
    const name = trimmed.split("=")[0] ?? trimmed;
    if (!CONFIG_FLAGS.has(name)) {
      continue;
    }

    extracted.push(arg);
    if (!trimmed.includes("=")) {
      const next = argv[index + 1];
      if (next && !next.startsWith("--")) {
        extracted.push(next);
        index += 1;
      }
    }
  }

  return extracted;
}

function hasFlag(args: ParsedArgs, name: string): boolean {
  return args.flags.has(name);
}

function getFlag(args: ParsedArgs, name: string): string | undefined {
  return args.flags.get(name)?.at(-1);
}

function requireFlag(args: ParsedArgs, name: string): string {
  const value = getFlag(args, name);
  if (!value || value === "true") {
    throw new Error(`--${name} is required`);
  }
  return value;
}

async function resolveReviewTarget(
  context: CliContext,
  value: string,
): Promise<{ projectId: string; repo: string; prNumber: number }> {
  const explicitProjectId = getFlag(context.args, "project");
  const projects = await listProjects(context);
  const parsed = parseOptionalRepoPullRequestRef(value);

  if (parsed.repo) {
    const project = resolveProjectForRepo(
      projects,
      parsed.repo,
      explicitProjectId,
    );
    return {
      projectId: project.id,
      repo: parsed.repo,
      prNumber: parsed.prNumber,
    };
  }

  const projectId = resolveExplicitOrCurrentProjectId(
    context,
    projects,
    explicitProjectId,
  );
  const project = projects.find((candidate) => candidate.id === projectId);
  if (!project?.repo) {
    throw new Error(`project ${projectId} is missing a configured repo`);
  }

  return {
    projectId,
    repo: project.repo,
    prNumber: parsed.prNumber,
  };
}

function resolveExplicitOrCurrentProjectId(
  context: CliContext,
  projects: ProjectSummary[],
  explicitProjectId?: string,
): string {
  if (explicitProjectId && explicitProjectId !== "true") {
    const explicitProject = projects.find(
      (project) => project.id === explicitProjectId,
    );
    if (!explicitProject) {
      throw new Error(`project not found: ${explicitProjectId}`);
    }
    return explicitProject.id;
  }

  return resolveProjectId(context, projects);
}

async function listProjects(context: CliContext): Promise<ProjectSummary[]> {
  const data = await context.client.get<{ items: ProjectSummary[] }>(
    "/api/v1/projects",
  );
  return data.items;
}

function resolveProjectForRepo(
  projects: ProjectSummary[],
  repo: string,
  explicitProjectId?: string,
): ProjectSummary {
  if (explicitProjectId && explicitProjectId !== "true") {
    const explicitProject = projects.find(
      (project) => project.id === explicitProjectId,
    );
    if (!explicitProject) {
      throw new Error(`project not found: ${explicitProjectId}`);
    }
    if (explicitProject.repo !== repo) {
      throw new Error(
        `project ${explicitProjectId} is configured for ${explicitProject.repo ?? "no repo"}, not ${repo}`,
      );
    }
    return explicitProject;
  }

  const matches = projects.filter((project) => project.repo === repo);
  if (matches.length === 0) {
    throw new Error(
      `--project is required (no project configured for repo ${repo})`,
    );
  }
  if (matches.length > 1) {
    throw new Error(
      `--project is required (multiple projects are configured for repo ${repo})`,
    );
  }
  return matches[0] as ProjectSummary;
}

async function resolveCommandProjectId(context: CliContext): Promise<string> {
  const explicitProjectId = getFlag(context.args, "project");
  if (explicitProjectId && explicitProjectId !== "true") {
    return explicitProjectId;
  }

  return resolveProjectId(context, await listProjects(context));
}

function resolveProjectId(
  context: CliContext,
  projects: ProjectSummary[],
): string {
  const cwd = normalizeComparablePath(context.cwd);
  const matches = projects.filter((project) =>
    isWithinProjectRepo(cwd, project.repoPath),
  );

  if (matches.length === 0) {
    throw new Error(`--project is required (no project matched cwd ${cwd})`);
  }

  const rankedMatches = matches
    .map((project) => ({
      project,
      normalizedRepoPath: normalizeComparablePath(project.repoPath),
    }))
    .sort(
      (left, right) =>
        right.normalizedRepoPath.length - left.normalizedRepoPath.length,
    );
  const match = rankedMatches[0];
  if (!match) {
    throw new Error(`--project is required (no project matched cwd ${cwd})`);
  }

  const ambiguousMatch = rankedMatches.find(
    (candidate) =>
      candidate.project.id !== match.project.id &&
      candidate.normalizedRepoPath.length === match.normalizedRepoPath.length,
  );
  if (ambiguousMatch) {
    throw new Error(
      `--project is required (multiple projects matched cwd ${cwd})`,
    );
  }

  return match.project.id;
}

function isWithinProjectRepo(cwd: string, repoPath: string): boolean {
  const normalizedRepoPath = normalizeComparablePath(repoPath);
  return (
    cwd === normalizedRepoPath || cwd.startsWith(`${normalizedRepoPath}${sep}`)
  );
}

function normalizeComparablePath(path: string): string {
  return resolve(path).replace(/^\/private/, "");
}

function parsePullRequestRef(value: string): PullRequestRef {
  const match = /^(?<repo>[^#]+)#(?<prNumber>\d+)$/.exec(value);
  if (!match?.groups) {
    throw new Error(`Invalid pull request reference: ${value}`);
  }

  const repo = match.groups.repo;
  const prNumber = match.groups.prNumber;
  if (!repo || !prNumber) {
    throw new Error(`Invalid pull request reference: ${value}`);
  }

  return {
    repo,
    prNumber: Number(prNumber),
  };
}

function parseOptionalRepoPullRequestRef(value: string): {
  repo?: string;
  prNumber: number;
} {
  const trimmed = value.trim();
  const repoQualified = /^(?<repo>[^#]+)#(?<prNumber>\d+)$/.exec(trimmed);
  if (repoQualified?.groups?.repo && repoQualified.groups.prNumber) {
    return {
      repo: repoQualified.groups.repo,
      prNumber: Number(repoQualified.groups.prNumber),
    };
  }

  if (/^\d+$/.test(trimmed)) {
    return { prNumber: Number(trimmed) };
  }

  throw new Error(`Invalid pull request reference: ${value}`);
}

function deriveProjectId(repoPath: string): string {
  const normalized = basename(repoPath)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return normalized || "project";
}

async function loadCliConfig(options: {
  argv: string[];
  env: Record<string, string | undefined>;
  cwd: string;
}): Promise<LoadedCliConfig> {
  const defaultConfigPath = join(homedir(), ".looper", "config.json");
  const configPath =
    readConfigArg(options.argv, "config") ??
    options.env.LOOPER_CONFIG ??
    defaultConfigPath;
  const fileConfig = await readJsonFile(configPath);

  return {
    config: {
      server: {
        host:
          readConfigArg(options.argv, "host") ??
          options.env.LOOPER_HOST ??
          readString(fileConfig, ["server", "host"]) ??
          "127.0.0.1",
        port:
          Number(
            readConfigArg(options.argv, "port") ?? options.env.LOOPER_PORT,
          ) ||
          readNumber(fileConfig, ["server", "port"]) ||
          4310,
        baseUrl: readString(fileConfig, ["server", "baseUrl"]),
        localToken: options.env.LOOPER_TOKEN,
      },
      daemon: {
        mode:
          readConfigArg(options.argv, "daemon-mode") ??
          options.env.LOOPER_DAEMON_MODE ??
          readString(fileConfig, ["daemon", "mode"]) ??
          "foreground",
        logDir:
          readConfigArg(options.argv, "log-dir") ??
          options.env.LOOPER_LOG_DIR ??
          readString(fileConfig, ["daemon", "logDir"]) ??
          join(homedir(), ".looper", "logs"),
      },
    },
    metadata: { configPath },
  };
}

function readConfigArg(argv: string[], name: string): string | undefined {
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (!arg || !arg.startsWith(`--${name}`)) {
      continue;
    }

    if (arg.includes("=")) {
      return arg.slice(arg.indexOf("=") + 1);
    }

    const next = argv[index + 1];
    return next && !next.startsWith("--") ? next : undefined;
  }

  return undefined;
}

async function readJsonFile(path: string): Promise<Record<string, unknown>> {
  try {
    return JSON.parse(await readFile(path, "utf8")) as Record<string, unknown>;
  } catch {
    return {};
  }
}

function readString(
  value: Record<string, unknown>,
  keys: [string, string],
): string | undefined {
  const nested = value[keys[0]];
  if (!nested || typeof nested !== "object" || Array.isArray(nested)) {
    return undefined;
  }

  const field = (nested as Record<string, unknown>)[keys[1]];
  return typeof field === "string" ? field : undefined;
}

function readNumber(
  value: Record<string, unknown>,
  keys: [string, string],
): number | undefined {
  const nested = value[keys[0]];
  if (!nested || typeof nested !== "object" || Array.isArray(nested)) {
    return undefined;
  }

  const field = (nested as Record<string, unknown>)[keys[1]];
  return typeof field === "number" ? field : undefined;
}

function formatError(error: unknown): string {
  if (error instanceof CliApiError) {
    return [error.message, error.code, error.requestId]
      .filter(Boolean)
      .join(" | ");
  }

  return error instanceof Error ? error.message : String(error);
}

if (import.meta.main) {
  const code = await runCli(process.argv.slice(2));
  process.exit(code);
}
