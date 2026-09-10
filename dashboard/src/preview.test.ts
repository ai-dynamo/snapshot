// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from "vitest";

import { loadPreviewMetadata, parsePreviewMetadata } from "./preview.ts";

const metadata = {
  formatVersion: 1,
  key: "pr-250",
  generatedAt: "2026-09-10T10:00:00.000Z",
  expiresAt: "2026-09-24T10:00:00.000Z",
  historyRecordCount: 20,
  previewRecordCount: 3,
  combinedRecordCount: 23,
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

describe("parsePreviewMetadata", () => {
  it("accepts an expiring pull request preview", () => {
    expect(parsePreviewMetadata(metadata)).toEqual(metadata);
  });

  it.each([
    { ...metadata, key: "../../main" },
    { ...metadata, expiresAt: "not-a-date" },
    { ...metadata, previewRecordCount: -1 },
    {
      ...metadata,
      source: { ...metadata.source, runUrl: "javascript:alert(1)" },
    },
  ])("rejects invalid metadata", (value) => {
    expect(() => parsePreviewMetadata(value)).toThrow();
  });
});

describe("loadPreviewMetadata", () => {
  it("treats a missing metadata file as the durable dashboard", async () => {
    const fetchImpl = vi.fn<typeof fetch>().mockResolvedValue(new Response(null, { status: 404 }));

    await expect(loadPreviewMetadata("https://example.test/", fetchImpl)).resolves.toBeNull();
  });

  it("accepts an explicit durable-dashboard marker", async () => {
    const fetchImpl = vi.fn<typeof fetch>().mockResolvedValue(new Response("null"));

    await expect(loadPreviewMetadata("https://example.test/", fetchImpl)).resolves.toBeNull();
  });
});
