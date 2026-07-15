import fs from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

const rootDir = path.dirname(fileURLToPath(import.meta.url));
const publicDir = path.resolve(rootDir, "public");

const PUBLIC_MIME: Record<string, string> = {
  ".ico": "image/x-icon",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".webmanifest": "application/manifest+json",
  ".json": "application/json",
};

function servePublicUnderBase() {
  return (
    req: IncomingMessage,
    res: ServerResponse,
    next: (err?: unknown) => void,
  ) => {
    const raw = req.url ?? "";
    const urlPath = raw.split("?")[0] ?? "";

    // base is /dashboard/; bare / or /dashboard (no slash) otherwise 404s with
    // "configured with a public base URL of /dashboard/".
    if (urlPath === "/" || urlPath === "") {
      res.statusCode = 302;
      res.setHeader("Location", "/dashboard/");
      res.end();
      return;
    }
    if (urlPath === "/dashboard") {
      const qs = raw.includes("?") ? raw.slice(raw.indexOf("?")) : "";
      res.statusCode = 302;
      res.setHeader("Location", `/dashboard/${qs}`);
      res.end();
      return;
    }

    // Vite serves public/ at host root in dev, but HTML links use /dashboard/*.
    // Also map root /favicon.ico for browser default requests.
    let rel: string | null = null;
    if (urlPath === "/favicon.ico") {
      rel = "favicon.ico";
    } else if (
      urlPath === "/apple-touch-icon.png" ||
      urlPath === "/apple-touch-icon-precomposed.png"
    ) {
      rel = "apple-touch-icon.png";
    } else if (urlPath.startsWith("/dashboard/")) {
      const candidate = urlPath.slice("/dashboard/".length);
      if (candidate && !candidate.includes("..") && !candidate.includes("/")) {
        rel = candidate;
      }
    }

    if (rel) {
      const file = path.join(publicDir, rel);
      if (fs.existsSync(file) && fs.statSync(file).isFile()) {
        const ext = path.extname(file).toLowerCase();
        res.setHeader(
          "Content-Type",
          PUBLIC_MIME[ext] ?? "application/octet-stream",
        );
        fs.createReadStream(file).pipe(res);
        return;
      }
    }

    next();
  };
}

export default defineConfig({
  base: "/dashboard/",
  plugins: [
    react(),
    tailwindcss(),
    {
      name: "dashboard-base-redirect",
      configureServer(server) {
        // Run before Vite's internal middleware so /dashboard/favicon.* resolves.
        server.middlewares.use(servePublicUnderBase());
      },
    },
  ],
  resolve: {
    alias: {
      "@": path.resolve(rootDir, "./src"),
    },
  },
  build: {
    outDir: "../../internal/dashboard/assets",
    emptyOutDir: true,
  },
  server: {
    open: "/dashboard/",
    proxy: {
      "/api": {
        target: "http://127.0.0.1:17310",
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
