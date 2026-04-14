import { randomUUID } from "node:crypto";

import type { Logger } from "../bootstrap/logger";
import {
  assertValidProjectId,
  deriveProjectIdFromRepoPath,
  normalizeDerivedProjectId,
} from "../config/index";
import type { GitHubPullRequestSummary } from "../infra/index";
import type { Store } from "../storage/store";
import type {
  ProjectRecord,
  PullRequestSnapshotRecord,
  WorktreeRecord,
} from "../storage/types";

export interface ProjectManagerGitGateway {
  listWorktrees(
    repoPath: string,
  ): Promise<
    Array<{ path: string; branch?: string; headSha?: string; bare: boolean }>
  >;
  detectGitHubRepo(repoPath: string): Promise<string | null>;
}

export interface ProjectManagerGitHubGateway {
  listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
  }): Promise<GitHubPullRequestSummary[]>;
  capturePullRequestSnapshot(input: {
    projectId: string;
    repo: string;
    prNumber: number;
    cwd?: string;
    capturedAt?: string;
  }): Promise<PullRequestSnapshotRecord>;
}

export interface AddProjectInput {
  id: string;
  name: string;
  repoPath: string;
  baseBranch: string;
  idSource?: "explicit" | "derived";
  worktreeRoot?: string | null;
  repo?: string | null;
}

export interface AddProjectResult {
  project: ProjectRecord;
  repo: string | null;
  discoveredPullRequests: number;
  discoveredWorktrees: number;
  warnings: string[];
}

export interface ProjectManagerOptions {
  store: Store;
  logger: Logger;
  git: ProjectManagerGitGateway;
  github: ProjectManagerGitHubGateway;
  now?: () => Date;
}

export class ProjectIdCollisionError extends Error {
  constructor(projectId: string) {
    super(
      `Derived project id collides with an existing explicit project: ${projectId}`,
    );
    this.name = "ProjectIdCollisionError";
  }
}

export class ProjectManager {
  private readonly now: () => Date;

  constructor(private readonly options: ProjectManagerOptions) {
    this.now = options.now ?? (() => new Date());
  }

  public async addProject(input: AddProjectInput): Promise<AddProjectResult> {
    const existing = this.options.store.projects.getById(input.id);
    const projectId = existing ? existing.id : this.normalizeProjectId(input);
    const normalizedExisting =
      existing ?? this.getNormalizedExisting(input, projectId);
    if (!normalizedExisting) {
      assertValidProjectId(projectId);
    }
    const warnings: string[] = [];
    const nowIso = this.now().toISOString();
    const detectedRepo = await this.detectRepo(
      input.repo ?? null,
      input.repoPath,
      warnings,
    );
    const project = this.upsertProject(
      { ...input, id: projectId },
      detectedRepo,
      normalizedExisting,
      nowIso,
    );

    const discoveredWorktrees = await this.discoverWorktrees(project, warnings);
    const discoveredPullRequests = await this.discoverPullRequests(
      project,
      detectedRepo,
      warnings,
    );

    return {
      project,
      repo: detectedRepo,
      discoveredPullRequests,
      discoveredWorktrees,
      warnings,
    };
  }

  private normalizeProjectId(input: AddProjectInput): string {
    if (input.idSource !== "derived") {
      return input.id;
    }

    if (input.id !== deriveProjectIdFromRepoPath(input.repoPath)) {
      return input.id;
    }

    if (!input.id.startsWith("legacy-id-")) {
      return input.id;
    }

    return normalizeDerivedProjectId(input.id);
  }

  private getNormalizedExisting(
    input: AddProjectInput,
    projectId: string,
  ): ProjectRecord | null {
    if (projectId === input.id) {
      return null;
    }

    const existing = this.options.store.projects.getById(projectId);
    if (!existing) {
      return null;
    }

    const metadata = parseMetadata(existing.metadataJson);
    if (metadata.normalizedDerivedId === true) {
      return existing;
    }

    throw new ProjectIdCollisionError(projectId);
  }

  private upsertProject(
    input: AddProjectInput,
    repo: string | null,
    existing: ProjectRecord | null,
    nowIso: string,
  ): ProjectRecord {
    const metadata = parseMetadata(existing?.metadataJson);
    const derivedProjectId = deriveProjectIdFromRepoPath(input.repoPath);
    const normalizedDerivedId =
      metadata.normalizedDerivedId === true ||
      (input.idSource === "derived" &&
        derivedProjectId.startsWith("legacy-id-") &&
        input.id === normalizeDerivedProjectId(derivedProjectId));
    const nextMetadata = {
      ...metadata,
      ...(normalizedDerivedId ? { normalizedDerivedId: true } : {}),
      repo,
      worktreeRoot: input.worktreeRoot ?? metadata.worktreeRoot ?? null,
      source: existing ? (metadata.source ?? "api") : "api",
    };
    const record: ProjectRecord = {
      id: input.id,
      name: input.name,
      repoPath: input.repoPath,
      baseBranch: input.baseBranch,
      archived: false,
      metadataJson: JSON.stringify(nextMetadata),
      createdAt: existing?.createdAt ?? nowIso,
      updatedAt: nowIso,
    };
    this.options.store.projects.upsert(record);
    return record;
  }

  private async detectRepo(
    explicitRepo: string | null,
    repoPath: string,
    warnings: string[],
  ): Promise<string | null> {
    if (explicitRepo) {
      return explicitRepo;
    }

    try {
      return await this.options.git.detectGitHubRepo(repoPath);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      this.options.logger.warn("failed to detect GitHub repo for project", {
        repoPath,
        message,
      });
      warnings.push(`Could not detect GitHub repo: ${message}`);
      return null;
    }
  }

  private async discoverWorktrees(
    project: ProjectRecord,
    warnings: string[],
  ): Promise<number> {
    try {
      const worktrees = await this.options.git.listWorktrees(project.repoPath);
      const nowIso = this.now().toISOString();
      let discovered = 0;

      for (const worktree of worktrees) {
        if (worktree.bare || !worktree.branch) {
          continue;
        }

        const existing = this.options.store.worktrees.getByBranch(
          project.id,
          worktree.branch,
        );
        const record: WorktreeRecord = {
          id: existing?.id ?? randomUUID(),
          projectId: project.id,
          repoPath: project.repoPath,
          worktreePath: worktree.path,
          branch: worktree.branch,
          baseBranch:
            existing?.baseBranch ?? project.baseBranch ?? worktree.branch,
          status: "active",
          headSha: worktree.headSha ?? existing?.headSha ?? null,
          metadataJson: JSON.stringify({ discovered: true }),
          createdAt: existing?.createdAt ?? nowIso,
          updatedAt: nowIso,
          cleanedAt: null,
        };
        this.options.store.worktrees.upsert(record);
        discovered += 1;
      }

      return discovered;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      this.options.logger.warn("failed to discover worktrees for project", {
        projectId: project.id,
        repoPath: project.repoPath,
        message,
      });
      warnings.push(`Could not discover worktrees: ${message}`);
      return 0;
    }
  }

  private async discoverPullRequests(
    project: ProjectRecord,
    repo: string | null,
    warnings: string[],
  ): Promise<number> {
    if (!repo) {
      return 0;
    }

    try {
      const pullRequests = await this.options.github.listOpenPullRequests({
        repo,
        cwd: project.repoPath,
      });
      let discovered = 0;

      for (const pullRequest of pullRequests) {
        if (
          pullRequest.isDraft ||
          normalizePrState(pullRequest.state) !== "open"
        ) {
          continue;
        }

        const snapshot = await this.options.github.capturePullRequestSnapshot({
          projectId: project.id,
          repo,
          prNumber: pullRequest.number,
          cwd: project.repoPath,
          capturedAt: this.now().toISOString(),
        });
        this.options.store.pullRequestSnapshots.upsert(snapshot);
        discovered += 1;
      }

      return discovered;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      this.options.logger.warn("failed to discover pull requests for project", {
        projectId: project.id,
        repo,
        message,
      });
      warnings.push(`Could not discover pull requests: ${message}`);
      return 0;
    }
  }
}

function parseMetadata(metadataJson?: string | null): Record<string, unknown> {
  if (!metadataJson) {
    return {};
  }

  try {
    const parsed = JSON.parse(metadataJson);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    // noop
  }

  return {};
}

function normalizePrState(state?: string): string {
  return state?.trim().toLowerCase() ?? "open";
}
