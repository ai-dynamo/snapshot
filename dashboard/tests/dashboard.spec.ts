// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "@playwright/test";

const EMPTY_MANIFEST = {
  manifestVersion: 1,
  historyFormatVersion: 1,
  supportedSchemaVersions: [1],
  recordCount: 0,
  newestResultAt: null,
  chunks: [],
};

test("loads the production bundle without leaving the origin", async ({ page, baseURL }) => {
  const requests: string[] = [];
  page.on("request", (request) => requests.push(request.url()));

  await page.goto("/");
  await expect(page.getByRole("status")).toContainText("Loaded 6 benchmark results");

  expect(requests.length).toBeGreaterThan(0);
  for (const url of requests) {
    expect(url.startsWith(`${baseURL}/`)).toBe(true);
  }
});

test("loads, filters, and exposes benchmark details", async ({ page }) => {
  await page.goto("/");

  await expect(page.getByRole("heading", { name: "Framework E2E benchmarks" })).toBeVisible();
  await expect(page.getByRole("status")).toContainText(
    "Loaded 6 benchmark results from 2 of 3 monthly indexes",
  );
  await expect(page.getByRole("status")).toContainText("newest result");
  await expect(page.locator(".chart-card canvas")).toHaveCount(3);
  await expect(page.locator("#latest-body tr")).toHaveCount(5);

  const restoreCard = page.locator(".chart-card", { hasText: "Restore to traffic" });
  await expect(restoreCard.locator(".failure-strip button")).toHaveCount(2);
  await expect(restoreCard.locator(".failure-strip")).toContainText(
    "RestoreRequested event not observed",
  );
  await expect(restoreCard.locator(".failure-strip")).toContainText("end event not reached");

  await page.getByLabel("SGLang").uncheck();
  await expect(page.locator("#latest-body")).not.toContainText("SGLang");

  await page.getByLabel("Date range").selectOption("30");
  await expect(page.locator("#latest-body tr")).toHaveCount(3);

  await page.getByRole("button", { name: "Details" }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Snapshot tag");
  await expect(dialog).toContainText("NVIDIA A100-SXM4-80GB");
  await expect(dialog).toContainText("previous");
  await expect(dialog.getByRole("link", { name: "Open GitHub Actions run" })).toHaveAttribute(
    "href",
    /actions\/runs\/\d+$/,
  );
  await expect(dialog.getByRole("link", { name: "Open commit" })).toHaveAttribute(
    "href",
    /\/commit\/[0-9a-f]+$/,
  );
  await page.getByRole("button", { name: "Close" }).click();

  await page.getByLabel("Outcome").selectOption("skipped");
  await expect(page.locator("#no-results")).toBeVisible();
  await expect(page.locator("#no-results")).toContainText("match the selected filters");
  await expect(page.locator("#latest-body tr")).toHaveCount(0);
});

test("loads older monthly chunks on demand", async ({ page }) => {
  const chunkRequests: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/index/v1/")) chunkRequests.push(request.url());
  });

  await page.goto("/");
  await expect(page.getByRole("status")).toContainText("1 older month load on demand");
  expect(chunkRequests.some((url) => url.endsWith("2026-06.ndjson"))).toBe(false);

  await page.getByLabel("Date range").selectOption("all");
  await expect(page.getByRole("status")).toContainText(
    "Loaded 8 benchmark results from 3 of 3 monthly indexes",
  );
  await expect(page.locator("#latest-body tr")).toHaveCount(6);
  expect(chunkRequests.some((url) => url.endsWith("2026-06.ndjson"))).toBe(true);
});

test("keeps filter selections when widening the date range and resets them on demand", async ({
  page,
}) => {
  await page.goto("/");
  await expect(page.getByRole("status")).toContainText("Loaded 6 benchmark results");

  const sglang = page.getByRole("checkbox", { name: "SGLang" });
  await sglang.uncheck();
  await page.getByLabel("Outcome").selectOption("passed");
  await page.getByLabel("Date range").selectOption("all");

  await expect(page.getByRole("status")).toContainText("3 of 3 monthly indexes");
  await expect(sglang).not.toBeChecked();
  await expect(page.getByLabel("Outcome")).toHaveValue("passed");
  await expect(page.locator("#latest-body")).not.toContainText("SGLang");

  await page.getByRole("button", { name: "Reset filters" }).click();

  await expect(page.getByLabel("Date range")).toHaveValue("90");
  await expect(page.getByLabel("Outcome")).toHaveValue("all");
  await expect(sglang).toBeChecked();
  await expect(page.locator("#latest-body")).toContainText("SGLang");
});

test("offers a suite whose runs all predate the eager window and loads it on selection", async ({
  page,
}) => {
  await page.goto("/");
  await expect(page.getByRole("status")).toContainText("Loaded 6 benchmark results");
  await expect(page.getByLabel("Suite").locator('option[value="quarterly-soak"]')).toHaveCount(1);

  await page.getByLabel("Suite").selectOption("quarterly-soak");

  await expect(page.getByRole("status")).toContainText(
    "Loaded 8 benchmark results from 3 of 3 monthly indexes",
  );
  await expect(page.getByRole("heading", { name: "Copy throughput" })).toBeVisible();
  await expect(page.locator("#latest-body tr")).toHaveCount(1);
  await expect(page.locator("#latest-body")).toContainText("Azure Files");
});

test("reports a failed on-demand load and keeps the view consistent", async ({ page }) => {
  await page.route("**/index/v1/2026-06.ndjson", (route) => route.abort());

  await page.goto("/");
  await expect(page.getByRole("status")).toContainText("Loaded 6 benchmark results");

  await page.getByLabel("Date range").selectOption("all");

  const status = page.getByRole("status");
  await expect(status).toContainText("Older benchmark history unavailable");
  await expect(status).toHaveClass(/status--error/);
  await expect(page.locator("#latest-body tr")).toHaveCount(5);

  await page.getByLabel("Date range").selectOption("90");
  await expect(status).not.toHaveClass(/status--error/);
  await expect(status).toContainText("Loaded 6 benchmark results");
});

test("keeps a month whose index returned an error pending and retries it", async ({ page }) => {
  let failing = true;
  await page.route("**/index/v1/2026-06.ndjson", async (route) => {
    if (failing) {
      await route.fulfill({ status: 502, body: "upstream error" });
    } else {
      await route.continue();
    }
  });

  await page.goto("/");
  await page.getByLabel("Date range").selectOption("all");

  const status = page.getByRole("status");
  await expect(status).toContainText("Could not load index/v1/2026-06.ndjson (502)");
  await expect(status).toContainText("from 2 of 3 monthly indexes");
  await expect(status).toHaveClass(/status--error/);
  await expect(page.locator("#latest-body tr")).toHaveCount(5);

  failing = false;
  await page.getByLabel("Date range").selectOption("180");
  await expect(status).toContainText("Loaded 8 benchmark results from 3 of 3 monthly indexes");
  await expect(status).not.toHaveClass(/status--error/);
});

test("discovers a new suite and metric without UI code changes", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("Suite").selectOption("storage-throughput");

  await expect(page.getByRole("heading", { name: "Copy throughput" })).toBeVisible();
  await expect(page.locator(".chart-card canvas")).toHaveCount(1);
  await expect(page.locator("#latest-body")).toContainText("Azure Files");
  await expect(page.locator("#latest-body")).toContainText("GiB/s");
});

test("explains an empty history before the first scheduled run", async ({ page }) => {
  await page.route("**/index/manifest.json", (route) => route.fulfill({ json: EMPTY_MANIFEST }));

  await page.goto("/");

  await expect(page.getByRole("status")).toContainText("Loaded 0 benchmark results");
  await expect(page.locator("#no-results")).toBeVisible();
  await expect(page.locator("#no-results")).toContainText(
    "No benchmark results have been published yet",
  );
  await expect(page.locator(".chart-card canvas")).toHaveCount(0);
  await expect(page.locator("#latest-body tr")).toHaveCount(0);
});

test("surfaces record warnings with their location", async ({ page }) => {
  await page.route("**/index/v1/2026-08.ndjson", (route) =>
    route.fulfill({ contentType: "application/x-ndjson", body: "{not json}\n" }),
  );

  await page.goto("/");

  await expect(page.getByRole("status")).toContainText("1 record warning");
  const warnings = page.locator("#load-warnings");
  await expect(warnings).toContainText("index/v1/2026-08.ndjson:1");
});
