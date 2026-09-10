// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from "vitest/config";

export default defineConfig({
  base: "./",
  // Fixture history is useful locally and in browser tests. Production Pages
  // builds replace it with the durable data-only history branch instead.
  publicDir: process.env.DASHBOARD_PRODUCTION === "true" ? false : "public",
  build: {
    outDir: "dist",
    sourcemap: true,
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
