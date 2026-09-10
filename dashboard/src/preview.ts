// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const PREVIEW_KEY = /^(?:pr|run)-[1-9][0-9]*$/;

export interface PreviewSource {
  event: string;
  branch: string;
  commit: string;
  runId: string;
  runAttempt: number;
  runUrl: string;
  pullRequest: number | null;
}

export interface PreviewMetadata {
  formatVersion: 1;
  key: string;
  generatedAt: string;
  expiresAt: string;
  historyRecordCount: number;
  previewRecordCount: number;
  combinedRecordCount: number;
  source: PreviewSource;
}

export class PreviewMetadataError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PreviewMetadataError";
  }
}

export async function loadPreviewMetadata(
  siteRoot: string | URL,
  fetchImpl: typeof fetch = globalThis.fetch,
): Promise<PreviewMetadata | null> {
  const root = new URL(siteRoot, globalThis.location?.href ?? "http://localhost/");
  const response = await fetchImpl(new URL("preview.json", root));
  if (response.status === 404) return null;
  if (!response.ok) {
    throw new PreviewMetadataError(
      `Could not load preview metadata (${response.status})`,
    );
  }
  const text = await response.text();
  if (text.trim() === "null") return null;
  return parsePreviewMetadata(text);
}

export function parsePreviewMetadata(value: unknown): PreviewMetadata {
  const parsed = typeof value === "string" ? parseJson(value) : value;
  requireObject(parsed, "preview");
  if (parsed.formatVersion !== 1) {
    throw new PreviewMetadataError(
      `Unsupported preview format ${String(parsed.formatVersion)}`,
    );
  }
  requireString(parsed.key, "preview.key");
  if (!PREVIEW_KEY.test(parsed.key)) {
    throw new PreviewMetadataError("preview.key is invalid");
  }
  requireTimestamp(parsed.generatedAt, "preview.generatedAt");
  requireTimestamp(parsed.expiresAt, "preview.expiresAt");
  for (const field of [
    "historyRecordCount",
    "previewRecordCount",
    "combinedRecordCount",
  ] as const) {
    requireNonnegativeInteger(parsed[field], `preview.${field}`);
  }
  requireObject(parsed.source, "preview.source");
  for (const field of ["event", "branch", "commit", "runId"] as const) {
    requireString(parsed.source[field], `preview.source.${field}`);
  }
  requirePositiveInteger(parsed.source.runAttempt, "preview.source.runAttempt");
  requireString(parsed.source.runUrl, "preview.source.runUrl");
  const url = new URL(parsed.source.runUrl);
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new PreviewMetadataError("preview.source.runUrl must use HTTP(S)");
  }
  if (
    parsed.source.pullRequest !== null &&
    parsed.source.pullRequest !== undefined
  ) {
    requirePositiveInteger(parsed.source.pullRequest, "preview.source.pullRequest");
  } else {
    parsed.source.pullRequest = null;
  }
  return parsed as unknown as PreviewMetadata;
}

function parseJson(value: string): unknown {
  try {
    return JSON.parse(value);
  } catch (error: unknown) {
    throw new PreviewMetadataError(
      `Preview metadata is not valid JSON: ${String(error)}`,
    );
  }
}

function requireObject(
  value: unknown,
  location: string,
): asserts value is Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new PreviewMetadataError(`${location} must be an object`);
  }
}

function requireString(
  value: unknown,
  location: string,
): asserts value is string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new PreviewMetadataError(`${location} must be a non-empty string`);
  }
}

function requirePositiveInteger(value: unknown, location: string): void {
  if (!Number.isInteger(value) || Number(value) < 1) {
    throw new PreviewMetadataError(`${location} must be a positive integer`);
  }
}

function requireNonnegativeInteger(value: unknown, location: string): void {
  if (!Number.isInteger(value) || Number(value) < 0) {
    throw new PreviewMetadataError(`${location} must be a nonnegative integer`);
  }
}

function requireTimestamp(value: unknown, location: string): void {
  requireString(value, location);
  if (!Number.isFinite(Date.parse(value))) {
    throw new PreviewMetadataError(`${location} must be an RFC3339 timestamp`);
  }
}
