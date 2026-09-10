// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from "node:fs";

const { engines } = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
const required = engines.node.replace(/^>=/, "").split(".").map(Number);
const current = process.versions.node.split(".").map(Number);

function compareVersions(left, right) {
  for (let index = 0; index < 3; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0);
    if (difference !== 0) return difference;
  }
  return 0;
}

if (compareVersions(current, required) < 0) {
  console.error(
    `Node.js ${engines.node} is required to run the dashboard scripts natively; ` +
      `found ${process.versions.node}. Use the version in .nvmrc (for example: nvm use).`,
  );
  process.exit(1);
}
