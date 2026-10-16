// Starts mockfix for the e2e tests with a copy of configs/mock.yaml on
// separate ports, in a temporary directory so the repo copy is untouched.
import { spawn } from "node:child_process";
import { cpSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const root = resolve(import.meta.dirname, "../..");
const dir = mkdtempSync(join(tmpdir(), "mockfix-e2e-"));
const cfg = readFileSync(join(root, "configs/mock.yaml"), "utf8")
  .replace("listen: 0.0.0.0:9876", "listen: 127.0.0.1:19876")
  .replace("listen: 127.0.0.1:9877", "listen: 127.0.0.1:19877")
  .replace("listen: 0.0.0.0:8080", "listen: 127.0.0.1:18080")
  .replace("data_dictionary: spec/FIX44.xml", `data_dictionary: ${join(root, "spec/FIX44.xml")}`)
  .replace("file: data/aapl.csv", `file: ${join(root, "data/aapl.csv")}`);
writeFileSync(join(dir, "mock.yaml"), cfg);
writeFileSync(join(root, "web/test-results-config-path"), join(dir, "mock.yaml"));
cpSync(join(root, "data"), join(dir, "data"), { recursive: true });

const child = spawn(join(root, "bin/mockfix"), ["--config", join(dir, "mock.yaml"), "--log-level", "warn"], {
  stdio: "inherit",
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => child.kill("SIGTERM"));
child.on("exit", (code) => process.exit(code ?? 0));
