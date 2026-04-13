#!/usr/bin/env bun

import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { basename, isAbsolute, join, relative, resolve } from "node:path";
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
}

interface CliContext {
  args: ParsedArgs;
  write: Writer;
  writeError: Writer;
  cwd: string;
  config: LoadedCliConfig;
  client: ApiClient;
  readFileImpl: (path: string, encoding: "utf8") => Promise<string>;
  showHelp: (commandName?: string) => void;
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
  repo?: string;
  prNumber: number;
}

interface ProjectSummary {
  id: string;
  repoPath: string;
  repo?: string | null;
  worktreeRoot?: string | null;
}

interface ActiveRunItem {
  runId: string;
  loopId: string;
  projectId: string;
  type: string;
  status: string;
  currentStep: string | null;
  startedAt: string;
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
      cwd,
      config,
      client,
      readFileImpl: deps.readFileImpl ?? readFile,
      showHelp: () => {},
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
    case "run":
      if (subcommand === "list") {
        return runRunList(context);
      }
      context.showHelp("run");
      return;
    case "ps":
      return runPs(context);
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
    .option(
      "--project <projectId>",
      "Project id (auto-detected from cwd when omitted)",
    )
    .option("--title <title>", "Task title")
    .option("--prompt <text>", "Implementation prompt")
    .option("--spec <path>", "Spec path")
    .option("--pr <ref>", "Start work on an existing spec PR")
    .option("--issue <ref>", "Start work from a planner issue")
    .option("--repo <repo>", "Repository slug")
    .option("--base-branch <branch>", "Base branch")
    .example(
      (name) =>
        `  $ ${name} work --project project_1 --title "Ship CLI" --spec specs/ship-cli.md`,
    )
    .example((name) => `  $ ${name} work --pr 42`)
    .example((name) => `  $ ${name} work --pr acme/looper#42`)
    .example((name) => `  $ ${name} work --issue 123`)
    .example((name) => `  $ ${name} work --issue acme/looper#123`)
    .action(async (options) => {
      await dispatch(createContext(runtime, ["work"], options));
    });

  cli
    .command("plan <issue>", "Create a planner run")
    .option(
      "--project <projectId>",
      "Project id (auto-detected from cwd when omitted)",
    )
    .example((name) => `  $ ${name} plan 123`)
    .example((name) => `  $ ${name} plan acme/looper#123`)
    .action(async (issue, options) => {
      await dispatch(createContext(runtime, ["plan", issue], options));
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
    .command("ps", "Show running loops")
    .option("--type <type>", "Filter by loop type")
    .option("--project <projectId>", "Filter by project id")
    .example((name) => `  $ ${name} ps`)
    .example((name) => `  $ ${name} ps --type reviewer --project project_1`)
    .action(async (options) => {
      await dispatch(createContext(runtime, ["ps"], options));
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

async function buildLoopCreateBody(context: CliContext) {
  const type = requireFlag(context.args, "type");
  const pr = getFlag(context.args, "pr");

  if (pr) {
    const ref = parsePullRequestRef(pr);
    const repo = requireRepoFromRef(ref, "pull request");
    const snapshot = await context.client.get<Record<string, unknown>>(
      `/api/v1/pull-requests/${encodeURIComponent(repo)}/${ref.prNumber}`,
    );
    return {
      projectId: snapshot.projectId,
      type,
      targetType: "pull_request",
      repo,
      prNumber: ref.prNumber,
      status: "running",
    };
  }

  throw new Error("loop start requires --pr <repo>#<number>");
}

async function runLoopPause(context: CliContext) {
  const loopId = context.args.positionals[2] ?? getFlag(context.args, "id");
  if (!loopId) {
    throw new Error("Usage: looper loop pause <loop-id>");
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
  const body = await buildWorkCreateBody(context);
  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/workers",
    body,
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

async function buildWorkCreateBody(context: CliContext) {
  const prompt = getFlag(context.args, "prompt");
  const specPath = getFlag(context.args, "spec");
  const pr = getFlag(context.args, "pr");
  const issue = getFlag(context.args, "issue");
  const modeCount =
    Number(Boolean(pr)) +
    Number(Boolean(issue)) +
    Number(Boolean(prompt || specPath));

  if (modeCount === 0) {
    throw new Error(
      "work requires one of: --prompt/--spec, --pr <repo>#<number>, or --issue <number>",
    );
  }
  if (modeCount > 1) {
    throw new Error(
      "work accepts only one input mode at a time: create-pr, --pr, or --issue",
    );
  }

  const title = getFlag(context.args, "title");
  const repo = getFlag(context.args, "repo");
  const baseBranch = getFlag(context.args, "base-branch");

  if (pr) {
    const ref = parseOptionalRepoNumberRef(pr, "pull request");
    const project = await resolveProjectForWork(context, {
      repo: ref.repo ?? repo,
      requireRepoMatch: Boolean(ref.repo),
    });
    const resolvedRepo = ref.repo ?? repo ?? project.repo;
    if (!resolvedRepo) {
      throw new Error(
        "Could not determine repo for pull request; pass --pr <repo>#<number>, --repo <repo>, or --project <projectId>",
      );
    }
    return {
      projectId: project.id,
      title,
      repo: resolvedRepo,
      prNumber: ref.prNumber,
      baseBranch,
    };
  }

  if (issue) {
    const ref = parseOptionalRepoNumberRef(issue, "issue");
    const project = await resolveProjectForWork(context, {
      repo: ref.repo ?? repo,
      requireRepoMatch: Boolean(ref.repo),
    });
    const resolvedRepo = ref.repo ?? repo ?? project.repo;
    if (!resolvedRepo) {
      throw new Error(
        "Could not determine repo for issue; pass --issue <repo>#<number>, --repo <repo>, or --project <projectId>",
      );
    }
    return {
      projectId: project.id,
      title,
      issueNumber: ref.prNumber,
      repo: resolvedRepo,
      baseBranch,
    };
  }

  const project = await resolveProjectForWork(context, { repo });

  return {
    projectId: project.id,
    title,
    prompt,
    specPath,
    repo,
    baseBranch,
  };
}

async function resolveProjectForWork(
  context: CliContext,
  hint?: { repo?: string; requireRepoMatch?: boolean },
): Promise<ProjectSummary> {
  const projects = await listProjects(context);
  const explicitProjectId = getFlag(context.args, "project");
  if (explicitProjectId) {
    const project = projects.find(
      (candidate) => candidate.id === explicitProjectId,
    );
    if (!project) {
      throw new Error(`Project not found: ${explicitProjectId}`);
    }
    if (
      hint?.repo &&
      hint.requireRepoMatch &&
      project.repo !== null &&
      project.repo !== hint.repo
    ) {
      throw new Error(
        `Project ${explicitProjectId} is for repo ${project.repo}, but ${hint.repo} was requested`,
      );
    }
    return project;
  }

  if (hint?.repo) {
    const matches = projects.filter((project) => project.repo === hint.repo);
    if (matches.length === 1) {
      const project = matches[0];
      if (project) {
        return project;
      }
    }
    if (matches.length > 1) {
      throw new Error(
        `Multiple projects match repo ${hint.repo}; pass --project <projectId>`,
      );
    }

    const projectFromCwd = inferProjectFromCwd(context.cwd, projects);
    if (
      projectFromCwd &&
      (projectFromCwd.repo == null || projectFromCwd.repo === hint.repo)
    ) {
      return projectFromCwd;
    }

    throw new Error(
      `Project not found for repo ${hint.repo}; pass --project <projectId>`,
    );
  }

  const projectFromCwd = inferProjectFromCwd(context.cwd, projects);
  if (projectFromCwd) {
    return projectFromCwd;
  }

  throw new Error(
    "Could not infer project from the current working directory; pass --project <projectId>",
  );
}

async function listProjects(context: CliContext): Promise<ProjectSummary[]> {
  const data = await context.client.get<{
    items: Array<Record<string, unknown>>;
  }>("/api/v1/projects");

  const projects: ProjectSummary[] = [];
  for (const project of data.items) {
    if (
      typeof project.id !== "string" ||
      typeof project.repoPath !== "string"
    ) {
      continue;
    }

    projects.push({
      id: project.id,
      repoPath: project.repoPath,
      repo: typeof project.repo === "string" ? project.repo : null,
      worktreeRoot:
        typeof project.worktreeRoot === "string" ? project.worktreeRoot : null,
    });
  }

  return projects;
}

function inferProjectFromCwd(
  cwdValue: string,
  projects: ProjectSummary[],
): ProjectSummary | null {
  const cwd = resolve(cwdValue);
  const matches = projects
    .map((project) => ({
      project,
      matchLength: Math.max(
        pathMatchLength(cwd, project.repoPath),
        typeof project.worktreeRoot === "string"
          ? pathMatchLength(cwd, project.worktreeRoot)
          : -1,
      ),
    }))
    .filter((project) => project.matchLength >= 0)
    .sort((left, right) => right.matchLength - left.matchLength);

  const bestMatch = matches[0];
  if (!bestMatch) {
    return null;
  }

  const equallySpecificMatches = matches.filter(
    (match) => match.matchLength === bestMatch.matchLength,
  );
  if (equallySpecificMatches.length > 1) {
    throw new Error(
      "Multiple projects match the current working directory; pass --project <projectId>",
    );
  }

  return bestMatch.project;
}

function pathMatchLength(candidatePath: string, rootPath: string): number {
  const absoluteRoot = resolve(rootPath);
  const relation = relative(absoluteRoot, candidatePath);
  if (
    relation === "" ||
    (!relation.startsWith("..") && !isAbsolute(relation))
  ) {
    return absoluteRoot.length;
  }

  return -1;
}

async function runPlannerCreate(context: CliContext) {
  const issueText = context.args.positionals[1];
  if (!issueText) {
    throw new Error("Usage: looper plan <issue|repo#issue>");
  }
  const repo = getFlag(context.args, "repo");
  const issueRef = parseOptionalRepoNumberRef(issueText, "issue");
  const project = await resolveProjectForWork(context, {
    repo: issueRef.repo ?? repo,
    requireRepoMatch: Boolean(issueRef.repo),
  });

  const data = await context.client.post<Record<string, unknown>>(
    "/api/v1/planners",
    {
      projectId: project.id,
      issueNumber: issueRef.prNumber,
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
  const repo = requireRepoFromRef(ref, "pull request");
  const data = await context.client.get<Record<string, unknown>>(
    `/api/v1/pull-requests/${encodeURIComponent(repo)}/${ref.prNumber}`,
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
  const repo = requireRepoFromRef(ref, "pull request");
  const data = await context.client.get<Record<string, unknown>>(
    `/api/v1/pull-requests/${encodeURIComponent(repo)}/${ref.prNumber}/status`,
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
    context.write("No running loops.");
    return;
  }

  printTable(
    context.write,
    data.items.map((item) => ({
      type: item.type,
      target: item.target.label,
      run: item.runId,
      step: item.currentStep ?? "-",
      agent: item.agent?.vendor ?? "-",
      pid: item.agent?.pid ?? "-",
      status: item.status,
      age: formatRelativeAge(item.startedAt),
    })),
  );
}

function formatRelativeAge(startedAt: string): string {
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

function parsePullRequestRef(value: string): PullRequestRef {
  const parsed = parseOptionalRepoNumberRef(value, "pull request");
  if (!parsed.repo) {
    throw new Error(`Invalid pull request reference: ${value}`);
  }

  return parsed;
}

function parseOptionalRepoNumberRef(
  value: string,
  label: "pull request" | "issue",
): PullRequestRef {
  const qualifiedMatch = /^(?<repo>[^#]+)#(?<number>\d+)$/.exec(value);
  if (qualifiedMatch?.groups) {
    const repo = qualifiedMatch.groups.repo;
    const numberValue = qualifiedMatch.groups.number;
    if (!repo || !numberValue) {
      throw new Error(`Invalid ${label} reference: ${value}`);
    }

    return {
      repo,
      prNumber: Number(numberValue),
    };
  }

  const numberValue = Number(value);
  if (Number.isInteger(numberValue) && numberValue > 0) {
    return {
      prNumber: numberValue,
    };
  }

  throw new Error(`Invalid ${label} reference: ${value}`);
}

function requireRepoFromRef(
  ref: PullRequestRef,
  label: "pull request" | "issue",
): string {
  if (!ref.repo) {
    throw new Error(`Missing repo for ${label} reference`);
  }

  return ref.repo;
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
