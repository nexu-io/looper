import { useCallback, useEffect, useMemo, useState } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { Shell } from "@/components/layout/Shell";
import { exchangeBootstrapCodeIfPresent } from "@/lib/api";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import { LoopDetailPage } from "@/pages/LoopDetail";
import { LoopsPage } from "@/pages/Loops";
import { OverviewPage } from "@/pages/Overview";
import { ProjectsPage } from "@/pages/Projects";

function resolveHostPort(): string {
  // Prefer the browser's authority as displayed in the address bar.
  if (typeof window !== "undefined" && window.location.host) {
    return window.location.host;
  }
  const { hostname, port } = window.location;
  if (port) {
    return `${hostname}:${port}`;
  }
  if (hostname === "localhost" || hostname === "127.0.0.1") {
    return `${hostname}:17310`;
  }
  return hostname;
}

export default function App() {
  const [bootstrapped, setBootstrapped] = useState(false);
  const [bootstrapError, setBootstrapError] = useState<string | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [version, setVersion] = useState<string | undefined>();
  const hostPort = useMemo(() => resolveHostPort(), []);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        await exchangeBootstrapCodeIfPresent();
      } catch (err) {
        if (!cancelled) {
          const message = err instanceof Error ? err.message : String(err);
          setBootstrapError(message);
        }
      } finally {
        if (!cancelled) {
          setBootstrapped(true);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const onHealthChange = useCallback(
    (nextHealthy: boolean | null, nextVersion?: string) => {
      setHealthy(nextHealthy);
      if (nextVersion) {
        setVersion(nextVersion);
      }
    },
    [],
  );

  if (!bootstrapped) {
    return (
      <div className="px-3 py-3 text-[12px] text-[var(--text-muted)]">
        Starting dashboard…
      </div>
    );
  }

  if (bootstrapError) {
    return (
      <div className="mx-auto flex max-w-lg flex-col gap-3 px-3 py-10">
        <h1 className="m-0 text-[16px] font-semibold">Bootstrap failed</h1>
        <p className="m-0 text-[var(--text-muted)]">
          Could not exchange the one-shot dashboard bootstrap code for a session
          token. This is not a daemon connectivity failure.
        </p>
        <pre className="m-0 overflow-auto rounded border border-[var(--border)] bg-[var(--bg-muted)] p-2 mono text-[12px] text-[var(--danger)]">
          {bootstrapError}
        </pre>
        <div className="rounded border border-[var(--border)] bg-[var(--bg-elevated)] p-3 text-[12px]">
          <p className="m-0 mb-1 font-medium">Recovery</p>
          <ol className="m-0 list-decimal pl-4 text-[var(--text-muted)]">
            <li>
              Re-open via CLI:{" "}
              <code className="mono text-[var(--text)]">looper dashboard</code>
            </li>
            <li>Ensure the bootstrap code was not already used or expired</li>
          </ol>
        </div>
      </div>
    );
  }

  return (
    <BrowserRouter basename="/dashboard">
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route
                element={
                  <Shell
                    hostPort={hostPort}
                    healthy={healthy}
                    version={version}
                    onHealthChange={onHealthChange}
                  />
                }
              >
                <Route
                  index
                  element={<OverviewPage onHealthChange={onHealthChange} />}
                />
                <Route path="running" element={<Navigate to="/loops" replace />} />
                <Route path="loops" element={<LoopsPage />} />
                <Route path="loops/:selector" element={<LoopDetailPage />} />
                <Route path="projects" element={<ProjectsPage />} />
                <Route path="*" element={<Navigate to="/" replace />} />
              </Route>
            </Routes>
          </ProjectFilterProvider>
        </DashboardDataProvider>
      </ToastProvider>
    </BrowserRouter>
  );
}
