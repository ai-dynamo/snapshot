// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import { assembleSite } from "./assemble-site.ts";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  const { rm } = await import("node:fs/promises");
  await Promise.all(
    temporaryDirectories.splice(0).map((path) => rm(path, { recursive: true, force: true })),
  );
});

describe("assembleSite", () => {
  it("publishes durable data at the root and each preview below it", async () => {
    const root = await temporaryRoot();
    const dist = resolve(root, "dist");
    await mkdir(resolve(dist, "assets"), { recursive: true });
    await writeFile(resolve(dist, "index.html"), "dashboard");
    await writeFile(resolve(dist, "assets", "app.js"), "javascript");
    const historyIndex = resolve(root, "history", "index");
    await writeIndex(historyIndex, 0);
    const preview = resolve(root, "preview-data", "pr-250");
    await writeIndex(resolve(preview, "index"), 0);
    await mkdir(preview, { recursive: true });
    await writeFile(
      resolve(preview, "preview.json"),
      JSON.stringify(previewMetadata("pr-250")),
    );
    const expired = resolve(root, "preview-data", "run-100");
    await writeIndex(resolve(expired, "index"), 0);
    await writeFile(
      resolve(expired, "preview.json"),
      JSON.stringify({
        ...previewMetadata("run-100"),
        expiresAt: "2026-09-09T10:00:00.000Z",
      }),
    );

    const published = await assembleSite({
      distDirectory: dist,
      historyIndexDirectory: historyIndex,
      previewsDirectory: resolve(root, "preview-data"),
      now: new Date("2026-09-10T12:00:00.000Z"),
    });

    expect(published).toEqual(["pr-250"]);
    expect(await readFile(resolve(dist, "index", "manifest.json"), "utf8")).toContain(
      '"recordCount":0',
    );
    expect(await readFile(resolve(dist, "previews", "pr-250", "index.html"), "utf8"))
      .toBe("dashboard");
    expect(await readFile(resolve(dist, "previews", "pr-250", "assets", "app.js"), "utf8"))
      .toBe("javascript");
    await expect(
      readFile(resolve(dist, "previews", "run-100", "index.html"), "utf8"),
    ).rejects.toMatchObject({ code: "ENOENT" });
  });

  it("rejects a preview directory with a path-like name", async () => {
    const root = await temporaryRoot();
    const dist = resolve(root, "dist");
    await mkdir(resolve(dist, "assets"), { recursive: true });
    await writeFile(resolve(dist, "index.html"), "dashboard");
    const historyIndex = resolve(root, "history", "index");
    await writeIndex(historyIndex, 0);
    await mkdir(resolve(root, "preview-data", "invalid"), { recursive: true });

    await expect(
      assembleSite({
        distDirectory: dist,
        historyIndexDirectory: historyIndex,
        previewsDirectory: resolve(root, "preview-data"),
        now: new Date("2026-09-10T12:00:00.000Z"),
      }),
    ).rejects.toThrow("Unsafe preview entry");
  });
});

async function temporaryRoot(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "snapshot-dashboard-"));
  temporaryDirectories.push(root);
  return root;
}

async function writeIndex(directory: string, recordCount: number): Promise<void> {
  await mkdir(directory, { recursive: true });
  await writeFile(
    resolve(directory, "manifest.json"),
    JSON.stringify({
      manifestVersion: 1,
      historyFormatVersion: 1,
      supportedSchemaVersions: [1],
      recordCount,
      chunks: [],
    }),
  );
}

function previewMetadata(key: string) {
  return {
    formatVersion: 1,
    key,
    generatedAt: "2026-09-10T10:00:00.000Z",
    expiresAt: "2026-09-24T10:00:00.000Z",
    historyRecordCount: 0,
    previewRecordCount: 1,
    combinedRecordCount: 1,
    source: {
      event: "push",
      branch: "pull-request/250",
      commit: "0123456789abcdef",
      runId: "12345",
      runAttempt: 1,
      runUrl: "https://github.com/ai-dynamo/snapshot/actions/runs/12345",
      pullRequest: 250,
    },
  };
}
