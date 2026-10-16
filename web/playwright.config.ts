import { defineConfig } from "@playwright/test";

// Runs against the real binary (make build first; `make e2e` does both).
export default defineConfig({
  testDir: "e2e",
  timeout: 60_000,
  // The tests share one mockfix and change its config: run them in order.
  workers: 1,
  use: { baseURL: "http://127.0.0.1:18080", trace: "retain-on-failure" },
  webServer: {
    command: "node e2e/serve.mjs",
    url: "http://127.0.0.1:18080/healthz",
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
