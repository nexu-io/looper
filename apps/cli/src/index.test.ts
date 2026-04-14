import { describe, expect, test } from "bun:test";

import { runCli } from "./index";

function createConfig() {
  return {
    config: {
      server: {
        host: "127.0.0.1",
        port: 4310,
        baseUrl: "http://127.0.0.1:4310",
      },
      daemon: { mode: "foreground", logDir: "/tmp/looper-logs" },
    },
    metadata: {
      configPath: "/tmp/config.json",
    },
  };
}

describe("runCli", () => {
  test("renders status as json", async () => {
    const lines: string[] = [];
    const exitCode = await runCli(["status", "--json"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_1",
            data: { service: { healthy: true } },
          }),
        ),
    });

    expect(exitCode).toBe(0);
    expect(lines.join("\n")).toContain('"healthy": true');
  });

  test("creates worker work item with spec path", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(
      [
        "work",
        "--project",
        "project_1",
        "--title",
        "Ship CLI",
        "--spec",
        "spec.md",
        "--prompt",
        "Implement CLI flow",
        "--repo",
        "acme/looper",
        "--base-branch",
        "main",
      ],
      {
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          requests.push({
            url: String(input),
            body: init?.body as string | null,
          });
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_2",
              data: { id: "loop_1", title: "Ship CLI", status: "running" },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain("/api/v1/workers");
    expect(requests[0]?.body).toContain('"prompt":"Implement CLI flow"');
    expect(requests[0]?.body).toContain('"specPath":"spec.md"');
  });

  test("creates worker work item from issue number", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(
      [
        "work",
        "--project",
        "project_1",
        "--issue",
        "123",
        "--repo",
        "acme/looper",
        "--base-branch",
        "main",
      ],
      {
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          requests.push({
            url: String(input),
            body: init?.body as string | null,
          });
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_2_issue",
              data: {
                id: "loop_2",
                title: "Implement acme/looper#123",
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain("/api/v1/workers");
    expect(requests[0]?.body).toContain('"projectId":"project_1"');
    expect(requests[0]?.body).toContain('"issueNumber":123');
    expect(requests[0]?.body).toContain('"repo":"acme/looper"');
    expect(requests[0]?.body).toContain('"baseBranch":"main"');
    expect(requests[0]?.body).not.toContain('"title":');
  });

  test("detects worker project from cwd when --project is omitted", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(
      ["work", "--issue", "123", "--repo", "acme/looper"],
      {
        cwd: "/tmp/repos/looper/packages/cli",
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          const url = String(input);
          requests.push({
            url,
            body: init?.body as string | null,
          });

          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_1",
                data: {
                  items: [
                    { id: "project_other", repoPath: "/tmp/repos/other" },
                    { id: "project_1", repoPath: "/tmp/repos/looper" },
                  ],
                },
              }),
            );
          }

          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_work_cwd_1",
              data: {
                id: "loop_2",
                title: "Implement acme/looper#123",
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(2);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.url).toContain("/api/v1/workers");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
  });

  test("errors when worker cwd does not match a project and --project is omitted", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(["work", "--issue", "123"], {
      cwd: "/tmp/repos/missing",
      stderr: (line) => errors.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_projects_2",
            data: {
              items: [{ id: "project_1", repoPath: "/tmp/repos/looper" }],
            },
          }),
        ),
    });

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain(
      "--project is required (no project matched cwd /tmp/repos/missing)",
    );
  });

  test("normalizes /private-prefixed cwd aliases when detecting worker project", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(
      ["work", "--issue", "123", "--repo", "acme/looper"],
      {
        cwd: "/private/tmp/repos/looper/packages/cli",
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          const url = String(input);
          requests.push({
            url,
            body: init?.body as string | null,
          });

          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_private_1",
                data: {
                  items: [{ id: "project_1", repoPath: "/tmp/repos/looper" }],
                },
              }),
            );
          }

          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_work_private_1",
              data: {
                id: "loop_private_1",
                title: "Implement acme/looper#123",
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(2);
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
  });

  test("errors when worker cwd matches multiple equally specific projects", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(
      ["work", "--issue", "123", "--repo", "acme/looper"],
      {
        cwd: "/tmp/repos/looper/packages/cli",
        stderr: (line) => errors.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async () =>
          new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_projects_ambiguous_1",
              data: {
                items: [
                  { id: "project_1", repoPath: "/tmp/repos/looper" },
                  { id: "project_2", repoPath: "/tmp/repos/looper" },
                ],
              },
            }),
          ),
      },
    );

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain(
      "--project is required (multiple projects matched cwd /tmp/repos/looper/packages/cli)",
    );
  });

  test("rejects invalid worker issue number", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(
      ["work", "--project", "project_1", "--issue", "123abc"],
      {
        stderr: (line) => errors.push(line),
        loadConfigImpl: async () => createConfig() as never,
      },
    );

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain("--issue must be a positive integer");
  });

  test("rejects combining worker issue with prompt or spec", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(
      [
        "work",
        "--project",
        "project_1",
        "--issue",
        "123",
        "--prompt",
        "Implement CLI flow",
      ],
      {
        stderr: (line) => errors.push(line),
        loadConfigImpl: async () => createConfig() as never,
      },
    );

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain(
      "--issue cannot be combined with --prompt or --spec",
    );
  });

  test("rejects valueless title in worker issue mode", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(
      ["work", "--project", "project_1", "--issue", "123", "--title"],
      {
        stderr: (line) => errors.push(line),
        loadConfigImpl: async () => createConfig() as never,
      },
    );

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain(
      "option `--title <title>` value is missing",
    );
  });

  test("creates reviewer loop from PR reference", async () => {
    const requests: string[] = [];
    const exitCode = await runCli(
      ["loop", "start", "--type", "reviewer", "--pr", "acme/looper#42"],
      {
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          const url = String(input);
          requests.push(`${init?.method ?? "GET"} ${url}`);

          if (url.endsWith("/api/v1/pull-requests/acme%2Flooper/42")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_3",
                data: {
                  projectId: "project_1",
                  repo: "acme/looper",
                  prNumber: 42,
                },
              }),
            );
          }

          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_4",
              data: { id: "loop_1", type: "reviewer", status: "running" },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests[0]).toContain(
      "GET http://127.0.0.1:4310/api/v1/pull-requests/acme%2Flooper/42",
    );
    expect(requests[1]).toContain("POST http://127.0.0.1:4310/api/v1/loops");
  });

  test("creates manual review task from repo-qualified PR reference", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["review", "acme/looper#42", "--loop"], {
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        const url = String(input);
        requests.push({ url, body: init?.body as string | null });

        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_projects_review_1",
              data: {
                items: [
                  {
                    id: "project_1",
                    repoPath: "/tmp/repos/looper",
                    repo: "acme/looper",
                  },
                ],
              },
            }),
          );
        }

        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_review_1",
            data: {
              id: "loop_review_1",
              projectId: "project_1",
              repo: "acme/looper",
              prNumber: 42,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.url).toContain("/api/v1/loops");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"prNumber":42');
    expect(requests[1]?.body).toContain('"followUpdates":true');
    expect(requests[1]?.body).toContain('"manual":true');
  });

  test("creates manual review task from local project PR number", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["review", "42"], {
      cwd: "/tmp/repos/looper/packages/cli",
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        const url = String(input);
        requests.push({ url, body: init?.body as string | null });

        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_projects_review_2",
              data: {
                items: [
                  {
                    id: "project_1",
                    repoPath: "/tmp/repos/looper",
                    repo: "acme/looper",
                  },
                ],
              },
            }),
          );
        }

        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_review_2",
            data: {
              id: "loop_review_2",
              projectId: "project_1",
              repo: "acme/looper",
              prNumber: 42,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"prNumber":42');
    expect(requests[1]?.body).toContain('"followUpdates":false');
  });

  test("uses explicit project for local PR number review targets", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["review", "42", "--project", "project_2"], {
      cwd: "/tmp/outside-repo",
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        const url = String(input);
        requests.push({ url, body: init?.body as string | null });

        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_projects_review_3",
              data: {
                items: [
                  {
                    id: "project_1",
                    repoPath: "/tmp/repos/looper",
                    repo: "acme/looper",
                  },
                  {
                    id: "project_2",
                    repoPath: "/tmp/repos/other",
                    repo: "acme/other",
                  },
                ],
              },
            }),
          );
        }

        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_review_3",
            data: {
              id: "loop_review_3",
              projectId: "project_2",
              repo: "acme/other",
              prNumber: 42,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.body).toContain('"projectId":"project_2"');
    expect(requests[1]?.body).toContain('"repo":"acme/other"');
    expect(requests[1]?.body).toContain('"prNumber":42');
  });

  test("creates planner work item from issue number", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(
      ["plan", "--project", "project_1", "--issue", "123"],
      {
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          requests.push({
            url: String(input),
            body: init?.body as string | null,
          });
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_plan_1",
              data: {
                id: "loop_plan_1",
                issueNumber: 123,
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests[0]?.url).toContain("/api/v1/planners");
    expect(requests[0]?.body).toContain('"issueNumber":123');
  });

  test("detects planner project from cwd when --project is omitted", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["plan", "--issue", "123"], {
      cwd: "/tmp/repos/looper",
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        const url = String(input);
        requests.push({
          url,
          body: init?.body as string | null,
        });

        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_projects_3",
              data: {
                items: [{ id: "project_1", repoPath: "/tmp/repos/looper" }],
              },
            }),
          );
        }

        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_plan_cwd_1",
            data: {
              id: "loop_plan_1",
              issueNumber: 123,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(2);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.url).toContain("/api/v1/planners");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
  });

  test("errors when planner cwd matches multiple equally specific projects", async () => {
    const errors: string[] = [];
    const exitCode = await runCli(["plan", "--issue", "123"], {
      cwd: "/tmp/repos/looper",
      stderr: (line) => errors.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_projects_ambiguous_2",
            data: {
              items: [
                { id: "project_1", repoPath: "/tmp/repos/looper" },
                { id: "project_2", repoPath: "/tmp/repos/looper" },
              ],
            },
          }),
        ),
    });

    expect(exitCode).toBe(1);
    expect(errors.at(-1)).toContain(
      "--project is required (multiple projects matched cwd /tmp/repos/looper)",
    );
  });

  test("adds project and requests discovery", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["project", "add", "/tmp/repos/looper"], {
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        requests.push({
          url: String(input),
          body: init?.body as string | null,
        });
        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_project_add",
            data: {
              id: "looper",
              name: "looper",
              repoPath: "/tmp/repos/looper",
              baseBranch: "main",
              repo: "powerformer/looper",
              discoveredPullRequests: 1,
              discoveredWorktrees: 2,
              warnings: [],
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[0]?.body).toContain('"id":"looper"');
    expect(requests[0]?.body).toContain('"name":"looper"');
    expect(requests[0]?.body).toContain('"repoPath":"/tmp/repos/looper"');
  });

  test("lists projects", async () => {
    const lines: string[] = [];
    const requests: string[] = [];
    const exitCode = await runCli(["project", "list"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input) => {
        requests.push(String(input));
        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_project_list",
            data: {
              items: [
                {
                  id: "looper",
                  name: "Looper",
                  repoPath: "/tmp/repos/looper",
                  baseBranch: "main",
                  repo: "powerformer/looper",
                  updatedAt: "2026-04-11T00:00:00.000Z",
                },
              ],
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]).toContain("/api/v1/projects");
    expect(lines.join("\n")).toContain("looper");
    expect(lines.join("\n")).toContain("/tmp/repos/looper");
  });

  test("shows daemon logs tail", async () => {
    const lines: string[] = [];
    const exitCode = await runCli(["daemon", "logs", "--lines", "1"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      readFileImpl: async () => "one\ntwo\n",
      fetchImpl: async () =>
        new Response(
          JSON.stringify({ ok: true, requestId: "req_5", data: {} }),
        ),
    });

    expect(exitCode).toBe(0);
    expect(lines.at(-1)).toBe("two");
  });

  test("lists pull requests with reviewer and fixer status", async () => {
    const lines: string[] = [];
    const exitCode = await runCli(["pr", "list"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_pr_list",
            data: {
              items: [
                {
                  repo: "acme/looper",
                  prNumber: 42,
                  title: "Runtime foundation",
                  reviewState: "changes_requested",
                  checksSummary: "green",
                  reviewer: "running",
                  fixer: "paused",
                },
                {
                  repo: "acme/looper",
                  prNumber: 77,
                  title: null,
                  reviewState: null,
                  checksSummary: null,
                  reviewer: "queued",
                  fixer: null,
                },
              ],
            },
          }),
        ),
    });

    expect(exitCode).toBe(0);
    const output = lines.join("\n");
    expect(output).toContain("reviewer");
    expect(output).toContain("fixer");
    expect(output).toContain("running");
    expect(output).toContain("paused");
    expect(output).toContain("queued");
  });

  test("prints active runs as json for ps --json", async () => {
    const lines: string[] = [];
    const requests: string[] = [];
    const exitCode = await runCli(["ps", "--json"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input) => {
        requests.push(String(input));
        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_ps_json",
            data: {
              items: [
                {
                  runId: "run_1",
                  loopId: "loop_1",
                  projectId: "project_1",
                  type: "planner",
                  status: "running",
                  currentStep: "plan",
                  startedAt: "2026-04-11T12:00:00.000Z",
                  target: {
                    type: "issue",
                    repo: "acme/looper",
                    issueNumber: 77,
                    label: "acme/looper#77",
                  },
                  agent: {
                    active: true,
                    activeCount: 1,
                    executionId: "agent_1",
                    vendor: "opencode",
                    pid: 1234,
                    startedAt: "2026-04-11T12:00:01.000Z",
                    lastHeartbeatAt: "2026-04-11T12:00:02.000Z",
                    heartbeatCount: 3,
                    status: "running",
                  },
                },
              ],
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]).toContain("/api/v1/runs/active");
    expect(lines.join("\n")).toContain('"runId": "run_1"');
    expect(lines.join("\n")).toContain('"type": "issue"');
    expect(lines.join("\n")).toContain('"issueNumber": 77');
    expect(lines.join("\n")).toContain('"activeCount": 1');
  });

  test("renders ps table with expected column order and values", async () => {
    const lines: string[] = [];
    const originalNow = Date.now;
    Date.now = () => Date.parse("2026-04-11T12:05:00.000Z");

    try {
      const exitCode = await runCli(["ps"], {
        stdout: (line) => lines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async () =>
          new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_ps_table",
              data: {
                items: [
                  {
                    runId: "run_worker_1",
                    loopId: "loop_worker_1",
                    projectId: "project_1",
                    type: "worker",
                    status: "running",
                    currentStep: "execute",
                    startedAt: "2026-04-11T12:00:00.000Z",
                    target: {
                      type: "project",
                      projectId: "project_1",
                      label: "project_1",
                    },
                    agent: {
                      active: true,
                      activeCount: 2,
                      executionId: "agent_2",
                      vendor: "opencode",
                      pid: 2222,
                      startedAt: "2026-04-11T12:00:01.000Z",
                      lastHeartbeatAt: "2026-04-11T12:00:02.000Z",
                      heartbeatCount: 4,
                      status: "running",
                    },
                  },
                ],
              },
            }),
          ),
      });

      expect(exitCode).toBe(0);
      expect(lines[0]).toContain("type");
      expect(lines[0]).toContain("target");
      expect(lines[0]).toContain("#");
      expect(lines[0]).toContain("step");
      expect(lines[0]).toContain("agent");
      expect(lines[0]).toContain("pid");
      expect(lines[0]).toContain("status");
      expect(lines[0]).toContain("age");
      expect(lines[2]).toContain("worker");
      expect(lines[2]).toContain("project_1");
      expect(lines[2]).toContain("execute");
      expect(lines[2]).toContain("opencode");
      expect(lines[2]).toContain("2222");
      expect(lines[2]).toContain("running");
      expect(lines[2]).toContain("5m");
    } finally {
      Date.now = originalNow;
    }
  });

  test("prints ps empty state", async () => {
    const lines: string[] = [];
    const exitCode = await runCli(["ps"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_ps_empty",
            data: { items: [] },
          }),
        ),
    });

    expect(exitCode).toBe(0);
    expect(lines).toEqual(["No running loops."]);
  });

  test("composes ps query params from --type and --project", async () => {
    const requests: string[] = [];
    const exitCode = await runCli(
      ["ps", "--type", "reviewer", "--project", "project_1"],
      {
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input) => {
          requests.push(String(input));
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_ps_query",
              data: { items: [] },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests[0]).toContain(
      "/api/v1/runs/active?type=reviewer&projectId=project_1",
    );
  });

  test("supports jump output modes", async () => {
    const requests: string[] = [];
    const fetchImpl = (async (input) => {
      requests.push(String(input));
      return new Response(
        JSON.stringify({
          ok: true,
          requestId: "req_jump",
          data: {
            seq: 12,
            loopId: "loop_12",
            projectId: "project_1",
            worktree: {
              id: "wt_12",
              path: "/tmp/looper-worktrees/loop-12",
              branch: "feature/loop-12",
            },
          },
        }),
      );
    }) as typeof fetch;

    const defaultLines: string[] = [];
    const defaultExit = await runCli(["jump", "12"], {
      isStdoutTty: false,
      stdout: (line) => defaultLines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl,
    });
    expect(defaultExit).toBe(0);
    expect(defaultLines.at(-1)).toBe("cd -- '/tmp/looper-worktrees/loop-12'");

    const pathLines: string[] = [];
    const pathExit = await runCli(["jump", "12", "--print-path"], {
      isStdoutTty: false,
      stdout: (line) => pathLines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl,
    });
    expect(pathExit).toBe(0);
    expect(pathLines).toEqual(["/tmp/looper-worktrees/loop-12"]);

    const jsonLines: string[] = [];
    const jsonExit = await runCli(["jump", "12", "--json"], {
      isStdoutTty: false,
      stdout: (line) => jsonLines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl,
    });
    expect(jsonExit).toBe(0);
    expect(jsonLines.join("\n")).toContain('"seq": 12');
    expect(jsonLines.join("\n")).toContain(
      '"path": "/tmp/looper-worktrees/loop-12"',
    );

    const shellLines: string[] = [];
    const shellExit = await runCli(
      ["jump", "12", "--shell-integration", "bash"],
      {
        isStdoutTty: false,
        stdout: (line) => shellLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      },
    );
    expect(shellExit).toBe(0);
    expect(shellLines).toEqual(['lj() { eval "$(looper jump "$@")"; }']);

    const shellNoIdLines: string[] = [];
    const shellNoIdExit = await runCli(
      ["jump", "--shell-integration", "bash"],
      {
        isStdoutTty: false,
        stdout: (line) => shellNoIdLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      },
    );
    expect(shellNoIdExit).toBe(0);
    expect(shellNoIdLines).toEqual(['lj() { eval "$(looper jump "$@")"; }']);

    expect(requests).toHaveLength(3);
    expect(requests[0]).toContain("/api/v1/runs/active/12");
  });

  test("jump opens an interactive shell when stdout is a tty", async () => {
    const launched: Array<{ cwd: string; shell?: string }> = [];
    const exitCode = await runCli(["jump", "12"], {
      env: { SHELL: "/bin/zsh" },
      isStdoutTty: true,
      stdout: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_jump_tty",
            data: {
              seq: 12,
              loopId: "loop_12",
              projectId: "project_1",
              worktree: {
                id: "wt_12",
                path: "/tmp/looper-worktrees/loop-12",
                branch: "feature/loop-12",
              },
            },
          }),
        ),
      launchShellImpl: async (options) => {
        launched.push({ cwd: options.cwd, shell: options.env.SHELL });
        return 0;
      },
    });

    expect(exitCode).toBe(0);
    expect(launched).toEqual([
      {
        cwd: "/tmp/looper-worktrees/loop-12",
        shell: "/bin/zsh",
      },
    ]);
  });

  test("supports logs output modes and no-agent message", async () => {
    const requests: string[] = [];
    const fetchImpl = (async (input) => {
      const url = String(input);
      requests.push(url);
      if (url.endsWith("/api/v1/loops/99/logs")) {
        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_logs_empty",
            data: {
              seq: 99,
              loopId: "loop_99",
              loopType: "worker",
              loopStatus: "running",
              run: null,
              agent: null,
            },
          }),
        );
      }

      return new Response(
        JSON.stringify({
          ok: true,
          requestId: "req_logs",
          data: {
            seq: 12,
            loopId: "loop_12",
            loopType: "worker",
            loopStatus: "running",
            run: {
              runId: "run_12",
              status: "running",
              currentStep: "execute",
            },
            agent: {
              executionId: "agent_12",
              vendor: "opencode",
              status: "running",
              pid: 888,
              stdout: "one\ntwo\nthree\n",
              stderr: "err-one\nerr-two\n",
            },
          },
        }),
      );
    }) as typeof fetch;

    const defaultLines: string[] = [];
    expect(
      await runCli(["logs", "12"], {
        stdout: (line) => defaultLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      }),
    ).toBe(0);
    expect(defaultLines.join("\n")).toContain("Loop #12 · worker · running");
    expect(defaultLines.join("\n")).toContain("three");

    const stderrLines: string[] = [];
    expect(
      await runCli(["logs", "12", "--stderr", "--tail", "1"], {
        stdout: (line) => stderrLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      }),
    ).toBe(0);
    expect(stderrLines.join("\n")).toContain("err-two");
    expect(stderrLines.join("\n")).not.toContain("err-one");

    const fullLines: string[] = [];
    expect(
      await runCli(["logs", "12", "--full"], {
        stdout: (line) => fullLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      }),
    ).toBe(0);
    expect(fullLines.join("\n")).toContain("one");
    expect(fullLines.join("\n")).toContain("three");

    const jsonLines: string[] = [];
    expect(
      await runCli(["logs", "12", "--json"], {
        stdout: (line) => jsonLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      }),
    ).toBe(0);
    expect(jsonLines.join("\n")).toContain('"seq": 12');
    expect(jsonLines.join("\n")).toContain('"executionId": "agent_12"');

    const noAgentLines: string[] = [];
    expect(
      await runCli(["logs", "99"], {
        stdout: (line) => noAgentLines.push(line),
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl,
      }),
    ).toBe(0);
    expect(noAgentLines.join("\n")).toContain(
      "No agent output for the current step.",
    );
  });

  test("stops active run using numeric selector", async () => {
    const requests: Array<{ url: string; method?: string }> = [];
    const lines: string[] = [];
    const exitCode = await runCli(["stop", "12"], {
      stdout: (line) => lines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input, init) => {
        requests.push({ url: String(input), method: init?.method });
        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_stop",
            data: {
              loopId: "loop_12",
              runId: "run_12",
              executionId: "agent_12",
              vendor: "opencode",
              pid: 888,
              stopped: true,
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[0]).toMatchObject({ method: "POST" });
    expect(requests[0]?.url).toContain("/api/v1/runs/active/12/stop");
    expect(lines.join("\n")).toContain("Loop stopped");
    expect(lines.join("\n")).toContain("loop_12");
  });

  test("returns a non-zero exit code when stop reports stopped false", async () => {
    const stdoutLines: string[] = [];
    const stderrLines: string[] = [];
    const exitCode = await runCli(["stop", "12"], {
      stdout: (line) => stdoutLines.push(line),
      stderr: (line) => stderrLines.push(line),
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_stop_failed",
            data: {
              loopId: "loop_12",
              runId: "run_12",
              executionId: "agent_12",
              vendor: "opencode",
              pid: 888,
              stopped: false,
            },
          }),
        ),
    });

    expect(exitCode).toBe(1);
    expect(stdoutLines.join("\n")).toContain("Loop stopped");
    expect(stdoutLines.join("\n")).toContain("stopped     : no");
    expect(stderrLines.join("\n")).toContain("Loop 12 could not be stopped");
  });
});
