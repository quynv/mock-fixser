import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// `npm run dev` proxies the API and the FIX WebSocket to a running mockfix.
const backend = process.env.MOCKFIX_URL ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "monaco-esm": fileURLToPath(new URL("node_modules/monaco-editor/esm/vs", import.meta.url)) },
  },
  build: { outDir: "dist", emptyOutDir: true, chunkSizeWarningLimit: 8192 },
  server: {
    proxy: {
      "/api": backend,
      "/metrics": backend,
      "/ws": { target: backend.replace(/^http/, "ws"), ws: true },
    },
  },
  test: { include: ["src/**/*.test.ts"] },
});
