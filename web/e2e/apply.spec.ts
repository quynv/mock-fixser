import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const configPath = () => readFileSync(resolve(import.meta.dirname, "../test-results-config-path"), "utf8").trim();

test("edit a symbol, see validation, apply (section 11.5)", async ({ page }) => {
  await page.goto("/#/symbols/EURUSD");
  const status = page.getByTestId("config-status");
  await expect(status).toContainText("applied");
  const version = Number((await status.locator(".badge").first().textContent())!.slice(1));

  // Live prices arrive over FIX through the gateway.
  await expect(page.locator(".chart").first()).toBeVisible();
  await expect(page.getByText("waiting for prices…")).toHaveCount(0, { timeout: 10_000 });

  const field = (label: string) => page.getByLabel(label, { exact: true });

  // An invalid edit: marked unsaved, error shown next to the field.
  await field("Max").fill("1.0");
  await field("Max").press("Enter");
  await expect(status).toContainText("unsaved");
  await expect(page.locator(".field", { has: page.getByLabel("Max", { exact: true }) })).toContainText(
    "must be greater than min",
  );
  await expect(status).toContainText("error");

  // Apply stays clickable and reports the errors.
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByRole("status")).toContainText("validation error");

  // Fix it, change the spread, apply.
  await field("Max").fill("1.1");
  await field("Max").press("Enter");
  // A value different from the current one, so reruns also change it.
  const spread = (await field("Spread").inputValue()) === "0.0002" ? "0.0003" : "0.0002";
  await field("Spread").fill(spread);
  await field("Spread").press("Enter");
  await expect(status).not.toContainText("error");
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByRole("status")).toContainText(`Applied version ${version + 1}`);
  await expect(status).toContainText("applied");
  await expect(status).toContainText(`v${version + 1}`);

  // The file holds the change and keeps its comments.
  const file = readFileSync(configPath(), "utf8");
  expect(file).toContain(`spread: ${spread}`);
  expect(file).toContain("# list_status | none");
});

test("someone else applies while there are unsaved edits", async ({ page, request }) => {
  await page.goto("/#/symbols/AAPL");
  const status = page.getByTestId("config-status");
  await expect(status).toContainText("applied");
  const field = (label: string) => page.getByLabel(label, { exact: true });
  await field("Spread").fill((await field("Spread").inputValue()) === "0.04" ? "0.06" : "0.04");
  await field("Spread").press("Enter");
  await expect(status).toContainText("unsaved");

  // Another client changes the config through the API.
  const cur = await request.get("/api/config.yaml");
  const etag = cur.headers()["etag"];
  const res = await request.put("/api/config", {
    data: (await cur.text()).replace(/history_ticks: \d+/, `history_ticks: ${4000 + Math.floor(Math.random() * 999)}`),
    headers: { "Content-Type": "application/yaml", "If-Match": etag },
  });
  expect(res.status()).toBe(200);

  await expect(page.getByText(/was applied elsewhere/)).toBeVisible({ timeout: 10_000 });
  await page.getByRole("button", { name: /Reload/ }).click();
  await expect(status).toContainText("applied");
});

test("rule tester runs against the draft", async ({ page }) => {
  await page.goto("/#/rules");
  const tester = page.locator(".panel", { hasText: "Rule tester" });
  await tester.locator(".field", { hasText: "symbol" }).locator("input").fill("AAPL");
  await tester.getByRole("button", { name: "Test" }).click();
  await expect(tester).toContainText("partial-then-fill-aapl");
  await expect(tester).toContainText("total 1150 ms");
});
