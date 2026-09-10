// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { parseChunk, parseManifest } from "../src/data.ts";

const indexDirectory = resolve(process.argv[2] || "public/index");
const manifest = parseManifest(
  await readFile(resolve(indexDirectory, "manifest.json"), "utf8"),
);
let records = 0;
let invalid = 0;
let unsupported = 0;
for (const chunk of manifest.chunks) {
  const relative = chunk.path.replace(/^index\//, "");
  const parsed = parseChunk(
    await readFile(resolve(indexDirectory, relative), "utf8"),
  );
  if (parsed.records.length + parsed.warnings.length !== chunk.recordCount) {
    throw new Error(
      `${chunk.path} record count does not match the manifest ` +
        `(${parsed.records.length + parsed.warnings.length} != ${chunk.recordCount})`,
    );
  }
  records += parsed.records.length;
  for (const warning of parsed.warnings) {
    if (warning.code === "unsupported-schema") {
      unsupported += 1;
      console.warn(`${chunk.path}:${warning.line}: ${warning.message}`);
    } else {
      invalid += 1;
      console.error(`${chunk.path}:${warning.line}: ${warning.message}`);
    }
  }
}
const indexedRecords = records + invalid + unsupported;
if (indexedRecords !== manifest.recordCount) {
  throw new Error(
    `Index record count does not match the manifest (${indexedRecords} != ${manifest.recordCount})`,
  );
}
if (invalid > 0) {
  throw new Error(`History contains ${invalid} invalid record${invalid === 1 ? "" : "s"}`);
}
console.log(`Validated ${records} dashboard record${records === 1 ? "" : "s"}.`);
