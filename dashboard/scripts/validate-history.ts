// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { parseChunk, parseManifest } from "../src/data.ts";

const args = process.argv.slice(2);
const strict = args.includes("--strict");
const indexDirectory = resolve(args.find((arg) => !arg.startsWith("--")) || "public/index");
const annotate = process.env.GITHUB_ACTIONS === "true";

const manifest = parseManifest(
  await readFile(resolve(indexDirectory, "manifest.json"), "utf8"),
);
let records = 0;
let invalid = 0;
let unsupported = 0;
for (const chunk of manifest.chunks) {
  const relative = chunk.path.replace(/^index\//, "");
  const text = await readFile(resolve(indexDirectory, relative), "utf8");
  const lines = text.split(/\r?\n/).filter((line) => line.trim() !== "");
  const parsed = parseChunk(text);
  if (parsed.records.length + parsed.warnings.length !== chunk.recordCount) {
    throw new Error(
      `${chunk.path} record count does not match the manifest ` +
        `(${parsed.records.length + parsed.warnings.length} != ${chunk.recordCount})`,
    );
  }
  records += parsed.records.length;
  for (const warning of parsed.warnings) {
    const rawPath = rawPathOf(lines[(warning.line ?? 1) - 1]);
    const location = `${chunk.path}:${warning.line}${rawPath ? ` (${rawPath})` : ""}`;
    if (warning.code === "unsupported-schema") {
      unsupported += 1;
      report("warning", `${location}: ${warning.message}`);
    } else {
      invalid += 1;
      report(strict ? "error" : "warning", `${location}: ${warning.message}`);
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
  const summary = `History contains ${invalid} invalid record${invalid === 1 ? "" : "s"}`;
  if (strict) {
    throw new Error(summary);
  }
  report("warning", `${summary}; the dashboard skips them at load time.`);
}
console.log(`Validated ${records} dashboard record${records === 1 ? "" : "s"}.`);

function rawPathOf(line: string | undefined): string | null {
  if (!line) return null;
  try {
    const entry = JSON.parse(line) as { rawPath?: unknown };
    return typeof entry.rawPath === "string" ? entry.rawPath : null;
  } catch {
    return null;
  }
}

function report(level: "warning" | "error", message: string): void {
  if (annotate) {
    console.log(`::${level}::${message}`);
  } else if (level === "error") {
    console.error(message);
  } else {
    console.warn(message);
  }
}
