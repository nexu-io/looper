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
          const url = String(input);
          requests.push({
            url,
            body: init?.body as string | null,
          });
          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_create_mode",
                data: {
                  items: [{ id: "project_1", repoPath: "/tmp/project" }],
                },
              }),
            );
          }
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
    expect(requests).toHaveLength(2);
    expect(requests[1]?.url).toContain("/api/v1/workers");
    expect(requests[1]?.body).toContain('"prompt":"Implement CLI flow"');
    expect(requests[1]?.body).toContain('"specPath":"spec.md"');
  });

  test("infers project from cwd for work create mode", async () => {
    const requests: Array<{
      method: string;
      url: string;
      body?: string | null;
    }> = [];
    const exitCode = await runCli(
      [
        "work",
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
        cwd: "/tmp/repos/looper/packages/cli",
        stdout: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input, init) => {
          const url = String(input);
          requests.push({
            method: init?.method ?? "GET",
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
                    {
                      id: "project_inferred",
                      repoPath: "/tmp/repos/looper",
                    },
                  ],
                },
              }),
            );
          }

          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_worker_inferred",
              data: {
                id: "loop_2",
                title: "Implement CLI flow",
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(requests[0]?.url).toContain("/api/v1/projects");
    expect(requests[1]?.url).toContain("/api/v1/workers");
    expect(requests[1]?.body).toContain('"projectId":"project_inferred"');
  });

  test("prefers a unique --repo match over an ambiguous cwd match", async () => {
    let workerRequestCount = 0;
    const exitCode = await runCli(
      [
        "work",
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
        cwd: "/tmp/repos/looper",
        stdout: () => {},
        stderr: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input) => {
          const url = String(input);
          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_ambiguous_cwd",
                data: {
                  items: [
                    {
                      id: "project_1",
                      repoPath: "/tmp/repos/project-one",
                      repo: "acme/looper",
                      worktreeRoot: "/tmp/repos/looper",
                    },
                    {
                      id: "project_2",
                      repoPath: "/tmp/repos/project-two",
                      repo: "acme/looper-alt",
                      worktreeRoot: "/tmp/repos/looper",
                    },
                  ],
                },
              }),
            );
          }

          workerRequestCount += 1;
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_worker_repo_match",
              data: {
                id: "loop_repo_match",
                title: "Implement CLI flow",
                status: "running",
              },
            }),
          );
        },
      },
    );

    expect(exitCode).toBe(0);
    expect(workerRequestCount).toBe(1);
  });

  test("fails work create when --repo has no matching project instead of falling back to cwd", async () => {
    let workerRequestCount = 0;
    const exitCode = await runCli(
      [
        "work",
        "--spec",
        "spec.md",
        "--prompt",
        "Implement CLI flow",
        "--repo",
        "acme/other",
        "--base-branch",
        "main",
      ],
      {
        cwd: "/tmp/repos/looper/packages/cli",
        stdout: () => {},
        stderr: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input) => {
          const url = String(input);
          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_missing_repo_hint",
                data: {
                  items: [
                    {
                      id: "project_cwd",
                      repoPath: "/tmp/repos/looper",
                      repo: "acme/looper",
                    },
                  ],
                },
              }),
            );
          }

          workerRequestCount += 1;
          return new Response(
            JSON.stringify({ ok: true, requestId: "unexpected" }),
          );
        },
      },
    );

    expect(exitCode).toBe(1);
    expect(workerRequestCount).toBe(0);
  });

  test("creates worker from numeric --pr input", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["work", "--pr", "42"], {
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
              requestId: "req_projects_2",
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
            requestId: "req_worker_pr",
            data: {
              id: "loop_worker_pr",
              title: "Implement acme/looper#42",
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.url).toContain("/api/v1/workers");
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"prNumber":42');
  });

  test("creates worker from qualified --pr input", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["work", "--pr", "acme/looper#42"], {
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
              requestId: "req_projects_qualified_pr",
              data: {
                items: [{ id: "project_1", repoPath: "/tmp/repos/looper" }],
              },
            }),
          );
        }

        return new Response(
          JSON.stringify({
            ok: true,
            requestId: "req_worker_qualified_pr",
            data: {
              id: "loop_worker_pr_qualified",
              title: "Implement acme/looper#42",
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"prNumber":42');
  });

  test("creates worker from numeric --issue input", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["work", "--issue", "123"], {
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
            requestId: "req_worker_issue",
            data: {
              id: "loop_worker_issue",
              title: "Implement acme/looper#123",
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.url).toContain("/api/v1/workers");
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
  });

  test("creates worker from qualified --issue input", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["work", "--issue", "acme/looper#123"], {
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
              requestId: "req_projects_qualified_issue",
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
            requestId: "req_worker_qualified_issue",
            data: {
              id: "loop_worker_issue_qualified",
              title: "Implement acme/looper#123",
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.body).toContain('"repo":"acme/looper"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
  });

  test("prefers qualified issue repo over cwd project", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["work", "--issue", "acme/other#123"], {
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
              requestId: "req_projects_qualified_issue_repo_priority",
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
            requestId: "req_worker_qualified_issue_repo_priority",
            data: {
              id: "loop_worker_issue_repo_priority",
              title: "Implement acme/other#123",
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.body).toContain('"projectId":"project_2"');
    expect(requests[1]?.body).toContain('"repo":"acme/other"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
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

  test("creates planner work item from numeric issue reference", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["plan", "123"], {
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
              requestId: "req_plan_projects_1",
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
            requestId: "req_plan_1",
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
    expect(requests[1]?.url).toContain("/api/v1/planners");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
  });

  test("creates planner work item from qualified issue reference", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["plan", "acme/looper#123"], {
      cwd: "/tmp/elsewhere",
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
              requestId: "req_plan_projects_2",
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
            requestId: "req_plan_2",
            data: {
              id: "loop_plan_2",
              issueNumber: 123,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.url).toContain("/api/v1/planners");
    expect(requests[1]?.body).toContain('"projectId":"project_1"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
  });

  test("prefers qualified planner issue repo over cwd project", async () => {
    const requests: Array<{ url: string; body?: string | null }> = [];
    const exitCode = await runCli(["plan", "acme/other#123"], {
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
              requestId: "req_plan_projects_repo_priority",
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
            requestId: "req_plan_repo_priority",
            data: {
              id: "loop_plan_repo_priority",
              issueNumber: 123,
              status: "running",
            },
          }),
        );
      },
    });

    expect(exitCode).toBe(0);
    expect(requests[1]?.url).toContain("/api/v1/planners");
    expect(requests[1]?.body).toContain('"projectId":"project_2"');
    expect(requests[1]?.body).toContain('"issueNumber":123');
  });

  test("rejects explicit project when qualified issue repo does not match", async () => {
    let workerRequestCount = 0;
    const exitCode = await runCli(
      ["work", "--issue", "acme/other#123", "--project", "project_1"],
      {
        cwd: "/tmp/repos/looper",
        stdout: () => {},
        stderr: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input) => {
          const url = String(input);
          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_projects_explicit_mismatch_issue",
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

          workerRequestCount += 1;
          return new Response(
            JSON.stringify({ ok: true, requestId: "unexpected" }),
          );
        },
      },
    );

    expect(exitCode).toBe(1);
    expect(workerRequestCount).toBe(0);
  });

  test("fails planner creation when qualified issue repo has no matching project", async () => {
    let plannerRequestCount = 0;
    const exitCode = await runCli(["plan", "acme/other#123"], {
      cwd: "/tmp/repos/looper",
      stdout: () => {},
      stderr: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input) => {
        const url = String(input);
        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_plan_projects_missing_qualified_repo",
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

        plannerRequestCount += 1;
        return new Response(
          JSON.stringify({ ok: true, requestId: "unexpected" }),
        );
      },
    });

    expect(exitCode).toBe(1);
    expect(plannerRequestCount).toBe(0);
  });

  test("fails planner creation when cwd matches multiple projects equally", async () => {
    let plannerRequestCount = 0;
    const exitCode = await runCli(["plan", "123"], {
      cwd: "/tmp/repos/looper",
      stdout: () => {},
      stderr: () => {},
      loadConfigImpl: async () => createConfig() as never,
      fetchImpl: async (input) => {
        const url = String(input);
        if (url.endsWith("/api/v1/projects")) {
          return new Response(
            JSON.stringify({
              ok: true,
              requestId: "req_plan_projects_ambiguous_cwd",
              data: {
                items: [
                  {
                    id: "project_1",
                    repoPath: "/tmp/repos/project-one",
                    repo: "acme/looper",
                    worktreeRoot: "/tmp/repos/looper",
                  },
                  {
                    id: "project_2",
                    repoPath: "/tmp/repos/project-two",
                    repo: "acme/looper-alt",
                    worktreeRoot: "/tmp/repos/looper",
                  },
                ],
              },
            }),
          );
        }

        plannerRequestCount += 1;
        return new Response(
          JSON.stringify({ ok: true, requestId: "unexpected" }),
        );
      },
    });

    expect(exitCode).toBe(1);
    expect(plannerRequestCount).toBe(0);
  });

  test("rejects explicit project when qualified planner issue repo does not match", async () => {
    let plannerRequestCount = 0;
    const exitCode = await runCli(
      ["plan", "acme/other#123", "--project", "project_1"],
      {
        cwd: "/tmp/repos/looper",
        stdout: () => {},
        stderr: () => {},
        loadConfigImpl: async () => createConfig() as never,
        fetchImpl: async (input) => {
          const url = String(input);
          if (url.endsWith("/api/v1/projects")) {
            return new Response(
              JSON.stringify({
                ok: true,
                requestId: "req_plan_projects_explicit_mismatch",
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

          plannerRequestCount += 1;
          return new Response(
            JSON.stringify({ ok: true, requestId: "unexpected" }),
          );
        },
      },
    );

    expect(exitCode).toBe(1);
    expect(plannerRequestCount).toBe(0);
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
      expect(lines[0]).toContain("run");
      expect(lines[0]).toContain("step");
      expect(lines[0]).toContain("agent");
      expect(lines[0]).toContain("pid");
      expect(lines[0]).toContain("status");
      expect(lines[0]).toContain("age");
      expect(lines[2]).toContain("worker");
      expect(lines[2]).toContain("project_1");
      expect(lines[2]).toContain("run_worker_1");
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
});
