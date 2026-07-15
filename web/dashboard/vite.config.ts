import path from "node:path";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

const rootDir = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  base: "/dashboard/",
  plugins: [
    react(),
    tailwindcss(),
    {
      name: "dashboard-base-redirect",
      configureServer(server) {
        // base is /dashboard/; bare / or /dashboard (no slash) otherwise 404s with
        // "configured with a public base URL of /dashboard/".
        server.middlewares.use((req, res, next) => {
          const url = req.url ?? "";
          if (url === "/" || url === "/?") {
            res.statusCode = 302;
            res.setHeader("Location", "/dashboard/");
            res.end();
            return;
          }
          if (url === "/dashboard" || url.startsWith("/dashboard?")) {
            const qs = url.includes("?") ? url.slice(url.indexOf("?")) : "";
            res.statusCode = 302;
            res.setHeader("Location", `/dashboard/${qs}`);
            res.end();
            return;
          }
          next();
        });
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
