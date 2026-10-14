// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { cp, lstat, mkdir, readFile, readdir, rm } from "node:fs/promises";
import { basename, dirname, relative, resolve, sep } from "node:path";

import { parseChunk, parseManifest } from "../src/data.ts";
import { parsePreviewMetadata } from "../src/preview.ts";

const PREVIEW_KEY = /^(?:pr|run)-[1-9][0-9]*$/;

export interface AssembleOptions {
  distDirectory: string;
  historyIndexDirectory: string;
  previewsDirectory?: string;
  now?: Date;
}

export async function assembleSite(options: AssembleOptions): Promise<string[]> {
  const dist = resolve(options.distDirectory);
  const historyIndex = resolve(options.historyIndexDirectory);
  await requireDirectory(dist, "dashboard distribution");
  await validateIndex(historyIndex);
  await replaceDirectory(historyIndex, resolve(dist, "index"));

  const previews = options.previewsDirectory
    ? resolve(options.previewsDirectory)
    : null;
  if (previews === null || !(await exists(previews))) return [];
  await requireDirectory(previews, "preview collection");

  const indexHtml = resolve(dist, "index.html");
  const assets = resolve(dist, "assets");
  const now = options.now ?? new Date();
  await requireRegularFile(indexHtml, "dashboard entry point");
  await requireDirectory(assets, "dashboard assets");
  const published: string[] = [];
  for (const entry of await readdir(previews, { withFileTypes: true })) {
    if (!entry.isDirectory() || entry.isSymbolicLink() || !PREVIEW_KEY.test(entry.name)) {
      throw new Error(`Unsafe preview entry: ${entry.name}`);
    }
    const source = resolve(previews, entry.name);
    requireInside(previews, source);
    await rejectSymlinks(source);
    const metadata = parsePreviewMetadata(
      await readFile(resolve(source, "preview.json"), "utf8"),
    );
    if (metadata.key !== entry.name) {
      throw new Error(`Preview metadata key does not match ${entry.name}`);
    }
    if (Date.parse(metadata.expiresAt) <= now.getTime()) continue;
    const sourceIndex = resolve(source, "index");
    await validateIndex(sourceIndex);

    const target = resolve(dist, "previews", entry.name);
    requireInside(resolve(dist, "previews"), target);
    await rm(target, { recursive: true, force: true });
    await mkdir(target, { recursive: true });
    await cp(indexHtml, resolve(target, "index.html"));
    await cp(assets, resolve(target, "assets"), { recursive: true });
    await cp(sourceIndex, resolve(target, "index"), { recursive: true });
    await cp(resolve(source, "preview.json"), resolve(target, "preview.json"));
    published.push(entry.name);
  }
  return published.sort();
}

export async function validateIndex(indexDirectory: string): Promise<void> {
  await requireDirectory(indexDirectory, "benchmark index");
  await rejectSymlinks(indexDirectory);
  const manifest = parseManifest(
    await readFile(resolve(indexDirectory, "manifest.json"), "utf8"),
  );
  let indexedRecords = 0;
  for (const chunk of manifest.chunks) {
    const relativeChunk = chunk.path.replace(/^index\//, "");
    const path = resolve(indexDirectory, relativeChunk);
    requireInside(indexDirectory, path);
    const parsed = parseChunk(await readFile(path, "utf8"));
    if (parsed.records.length + parsed.warnings.length !== chunk.recordCount) {
      throw new Error(`${chunk.path} record count does not match its manifest`);
    }
    const invalid = parsed.warnings.filter(
      (warning) => warning.code !== "unsupported-schema",
    );
    if (invalid.length > 0) {
      throw new Error(`${chunk.path} contains ${invalid.length} invalid records`);
    }
    indexedRecords += parsed.records.length + parsed.warnings.length;
  }
  if (indexedRecords !== manifest.recordCount) {
    throw new Error("Index record count does not match its manifest");
  }
}

async function replaceDirectory(source: string, target: string): Promise<void> {
  await rm(target, { recursive: true, force: true });
  await mkdir(dirname(target), { recursive: true });
  await cp(source, target, { recursive: true });
}

async function rejectSymlinks(root: string): Promise<void> {
  const rootInfo = await lstat(root);
  if (rootInfo.isSymbolicLink()) throw new Error(`Symlinks are not allowed: ${root}`);
  if (!rootInfo.isDirectory()) return;
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const path = resolve(root, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`Symlinks are not allowed: ${path}`);
    if (entry.isDirectory()) await rejectSymlinks(path);
  }
}

async function requireDirectory(path: string, label: string): Promise<void> {
  const info = await lstat(path);
  if (info.isSymbolicLink() || !info.isDirectory()) {
    throw new Error(`${label} is not a safe directory: ${path}`);
  }
}

async function requireRegularFile(path: string, label: string): Promise<void> {
  const info = await lstat(path);
  if (info.isSymbolicLink() || !info.isFile()) {
    throw new Error(`${label} is not a safe file: ${path}`);
  }
}

function requireInside(root: string, target: string): void {
  const path = relative(resolve(root), resolve(target));
  if (path === "" || (!path.startsWith(`..${sep}`) && path !== ".." && !path.startsWith(sep))) {
    return;
  }
  throw new Error(`${basename(target)} escapes ${root}`);
}

async function exists(path: string): Promise<boolean> {
  try {
    await lstat(path);
    return true;
  } catch (error: unknown) {
    if (
      typeof error === "object" &&
      error !== null &&
      "code" in error &&
      error.code === "ENOENT"
    ) {
      return false;
    }
    throw error;
  }
}

async function main(): Promise<void> {
  const args = new Map<string, string>();
  for (let index = 2; index < process.argv.length; index += 2) {
    const name = process.argv[index];
    const value = process.argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error("Arguments must be supplied as --name value pairs");
    }
    args.set(name, value);
  }
  const distDirectory = args.get("--dist");
  const historyIndexDirectory = args.get("--history-index");
  if (!distDirectory || !historyIndexDirectory) {
    throw new Error("--dist and --history-index are required");
  }
  const previewsDirectory = args.get("--previews");
  const published = await assembleSite({
    distDirectory,
    historyIndexDirectory,
    ...(previewsDirectory ? { previewsDirectory } : {}),
  });
  console.log(
    `Assembled dashboard with ${published.length} preview${published.length === 1 ? "" : "s"}.`,
  );
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(import.meta.filename)) {
  await main();
}
