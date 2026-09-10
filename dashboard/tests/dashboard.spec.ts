// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "@playwright/test";

test("loads, filters, and exposes benchmark details", async ({ page }) => {
  await page.goto("/");

  await expect(page.getByRole("heading", { name: "Framework E2E benchmarks" })).toBeVisible();
  await expect(page.getByRole("status")).toContainText("Loaded 6 benchmark results");
  await expect(page.locator(".chart-card canvas")).toHaveCount(3);
  await expect(page.locator("#latest-body tr")).toHaveCount(4);

  await page.getByLabel("SGLang").uncheck();
  await expect(page.locator("#latest-body")).not.toContainText("SGLang");

  await page.getByLabel("Date range").selectOption("30");
  await expect(page.locator("#latest-body tr")).toHaveCount(2);

  await page.getByRole("button", { name: "Details" }).first().click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("dialog")).toContainText("Snapshot tag");
  await expect(page.getByRole("dialog")).toContainText("NVIDIA A100-SXM4-80GB");
  await page.getByRole("button", { name: "Close" }).click();

  await page.getByLabel("Outcome").selectOption("skipped");
  await expect(page.locator("#no-results")).toBeVisible();
  await expect(page.locator("#latest-body tr")).toHaveCount(0);
});

test("discovers a new suite and metric without UI code changes", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("Suite").selectOption("storage-throughput");

  await expect(page.getByRole("heading", { name: "Copy throughput" })).toBeVisible();
  await expect(page.locator(".chart-card canvas")).toHaveCount(1);
  await expect(page.locator("#latest-body")).toContainText("Azure Files");
});
