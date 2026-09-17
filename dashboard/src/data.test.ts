// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import {
  DashboardDataError,
  TEST_TOTAL_METRIC,
  TOTAL_NOT_COMPARABLE,
  caseColorIndex,
  comparableStats,
  compareResults,
  comparisonKey,
  deriveComparisonKey,
  discoverDimensions,
  filterRecords,
  formatValue,
  loadHistory,
  loadRemainingHistory,
  newestResultAt,
  parseChunk,
  parseManifest,
  seriesForMetric,
  suiteNeedsPendingChunks,
  validateResult,
} from "./data.ts";
import type { BenchmarkResult, HistoryManifest, Measurement, Outcome } from "./data.ts";

const SHARED_COMPARISON_KEY = JSON.parse(
  readFileSync(new URL("../../e2e/tests/data/comparison-key.json", import.meta.url), "utf8"),
) as { comparisonKey: string; result: unknown };

describe("history parsing", () => {
  it("accepts a monthly manifest and rejects paths outside the index", () => {
    const manifest = parseManifest({
      manifestVersion: 1,
      historyFormatVersion: 1,
      supportedSchemaVersions: [1],
      recordCount: 1,
      newestResultAt: "2026-09-09T01:01:30.000Z",
      chunks: [
        {
          path: "index/v1/2026-09.ndjson",
          month: "2026-09",
          recordCount: 1,
          firstStartedAt: "2026-09-09T01:00:00.000Z",
          lastStartedAt: "2026-09-09T01:00:00.000Z",
        },
      ],
    });

    expect(manifest.recordCount).toBe(1);
    expect(() =>
      parseManifest({
        ...manifest,
        chunks: [{ ...manifest.chunks[0]!, path: "../../private.json" }],
      }),
    ).toThrow(DashboardDataError);
  });

  it("accepts per-chunk suites and rejects non-string entries", () => {
    const chunk = {
      path: "index/v1/2026-09.ndjson",
      recordCount: 1,
      firstStartedAt: "2026-09-09T01:00:00.000Z",
      lastStartedAt: "2026-09-09T01:00:00.000Z",
    };
    const base = {
      manifestVersion: 1,
      historyFormatVersion: 1,
      supportedSchemaVersions: [1],
      recordCount: 1,
      newestResultAt: null,
    };

    expect(parseManifest({ ...base, chunks: [chunk] }).chunks[0]!.suites).toBeUndefined();
    expect(
      parseManifest({ ...base, chunks: [{ ...chunk, suites: ["a", "b"] }] }).chunks[0]!.suites,
    ).toEqual(["a", "b"]);
    expect(() => parseManifest({ ...base, chunks: [{ ...chunk, suites: "a" }] })).toThrow(
      DashboardDataError,
    );
    expect(() => parseManifest({ ...base, chunks: [{ ...chunk, suites: ["a", 1] }] })).toThrow(
      DashboardDataError,
    );
  });

  it("keeps valid partial failures and skips unknown schema versions", () => {
    const failed = result({ outcome: "failed", value: null });
    const unknown = result({ runId: "2", schemaVersion: 99 });
    const parsed = parseChunk(
      [failed, unknown]
        .map((entry) => JSON.stringify({ rawPath: "result.json", result: entry }))
        .join("\n"),
    );

    expect(parsed.records).toHaveLength(1);
    expect(parsed.records[0]!.outcome).toBe("failed");
    expect(parsed.records[0]!.measurements[0]!.value).toBeNull();
    expect(parsed.warnings).toMatchObject([{ code: "unsupported-schema", line: 2 }]);
  });

  it("requires RFC3339 timestamps rather than anything Date.parse accepts", () => {
    for (const startedAt of ["1", "Sep 9 2026", "2026-09-09", "2026-09-09T01:00:00"]) {
      expect(() => validateResult(result({ startedAt }), new Set([1]))).toThrow(DashboardDataError);
    }
    expect(() =>
      validateResult(result({ startedAt: "2026-09-09T01:00:00+02:00" }), new Set([1])),
    ).not.toThrow();
  });

  it("orders results by parsed time, then identity", () => {
    const later = result({ runId: "1", startedAt: "2026-09-09T01:00:00.000Z" });
    const earlier = result({ runId: "9", startedAt: "2026-09-09T00:30:00.000+00:00" });
    const sameTime = result({ runId: "10", startedAt: "2026-09-09T01:00:00Z" });

    expect(compareResults(earlier, later)).toBeLessThan(0);
    expect([later, sameTime].sort(compareResults).map((item) => item.identity.runId)).toEqual([
      "1",
      "10",
    ]);
  });
});

describe("history loading", () => {
  const manifest: HistoryManifest = {
    manifestVersion: 1,
    historyFormatVersion: 1,
    supportedSchemaVersions: [1],
    recordCount: 3,
    newestResultAt: "2026-09-09T01:01:00.000Z",
    chunks: [
      {
        path: "index/v1/2026-09.ndjson",
        recordCount: 1,
        firstStartedAt: "2026-09-09T01:00:00.000Z",
        lastStartedAt: "2026-09-09T01:00:00.000Z",
      },
      {
        path: "index/v1/2026-08.ndjson",
        recordCount: 1,
        firstStartedAt: "2026-08-01T01:00:00.000Z",
        lastStartedAt: "2026-08-01T01:00:00.000Z",
      },
      {
        path: "index/v1/2026-03.ndjson",
        recordCount: 1,
        firstStartedAt: "2026-03-01T01:00:00.000Z",
        lastStartedAt: "2026-03-01T01:00:00.000Z",
      },
    ],
  };
  const chunks: Record<string, BenchmarkResult> = {
    "index/v1/2026-09.ndjson": result({ runId: "3", startedAt: "2026-09-09T01:00:00.000Z" }),
    "index/v1/2026-08.ndjson": result({ runId: "2", startedAt: "2026-08-01T01:00:00.000Z" }),
    "index/v1/2026-03.ndjson": result({ runId: "1", startedAt: "2026-03-01T01:00:00.000Z" }),
  };

  function fakeFetch(requested: string[]): typeof fetch {
    return (async (input: string | URL | Request) => {
      const url = new URL(input instanceof Request ? input.url : input);
      requested.push(url.pathname);
      if (url.pathname.endsWith("manifest.json")) {
        return new Response(JSON.stringify(manifest));
      }
      const record = chunks[url.pathname.replace(/^\//, "")];
      if (!record) return new Response("missing", { status: 404 });
      return new Response(JSON.stringify({ rawPath: "raw.json", result: record }));
    }) as typeof fetch;
  }

  it("fetches only the chunks in the recent window and loads the rest on demand", async () => {
    const requested: string[] = [];
    const recent = await loadHistory("http://dashboard.test/", fakeFetch(requested), {
      recentDays: 90,
    });

    expect(recent.records.map((item) => item.identity.runId)).toEqual(["2", "3"]);
    expect(recent.pendingChunks.map((chunk) => chunk.path)).toEqual(["index/v1/2026-03.ndjson"]);
    expect(requested).not.toContain("/index/v1/2026-03.ndjson");
    expect(newestResultAt(recent)).toBe(chunks["index/v1/2026-09.ndjson"]!.finishedAt);

    const complete = await loadRemainingHistory(recent, fakeFetch(requested));

    expect(complete.records.map((item) => item.identity.runId)).toEqual(["1", "2", "3"]);
    expect(complete.pendingChunks).toEqual([]);
    expect(requested).toContain("/index/v1/2026-03.ndjson");
  });

  it("loads every chunk when no window is requested", async () => {
    const requested: string[] = [];
    const all = await loadHistory("http://dashboard.test/", fakeFetch(requested), {
      recentDays: null,
    });

    expect(all.records).toHaveLength(3);
    expect(all.pendingChunks).toEqual([]);
  });

  it("keeps chunks whose fetch returned an error pending and retries them later", async () => {
    let failing = new Set(["index/v1/2026-08.ndjson", "index/v1/2026-03.ndjson"]);
    const requested: string[] = [];
    const flaky = (async (input: string | URL | Request) => {
      const url = new URL(input instanceof Request ? input.url : input);
      if (failing.has(url.pathname.replace(/^\//, ""))) {
        return new Response("upstream error", { status: 502 });
      }
      return fakeFetch(requested)(input);
    }) as typeof fetch;

    // An eager chunk that fails stays pending rather than counting as loaded.
    const recent = await loadHistory("http://dashboard.test/", flaky, { recentDays: 90 });
    expect(recent.records.map((item) => item.identity.runId)).toEqual(["3"]);
    expect(recent.pendingChunks.map((chunk) => chunk.path)).toEqual([
      "index/v1/2026-08.ndjson",
      "index/v1/2026-03.ndjson",
    ]);
    expect(recent.warnings).toEqual([]);
    expect(recent.loadFailures).toMatchObject([
      { code: "network", path: "index/v1/2026-08.ndjson" },
    ]);

    // A failed on-demand load keeps the failed month pending too.
    const attempted = await loadRemainingHistory(recent, flaky);
    expect(attempted.records.map((item) => item.identity.runId)).toEqual(["3"]);
    expect(attempted.pendingChunks).toEqual(recent.pendingChunks);
    expect(attempted.loadFailures).toHaveLength(2);

    failing = new Set();
    const complete = await loadRemainingHistory(attempted, flaky);
    expect(complete.records.map((item) => item.identity.runId)).toEqual(["1", "2", "3"]);
    expect(complete.pendingChunks).toEqual([]);
    expect(complete.loadFailures).toEqual([]);
  });

  it("asks for pending chunks only when the selected suite's window reaches them", async () => {
    const recent = await loadHistory("http://dashboard.test/", fakeFetch([]), {
      recentDays: 90,
    });
    const pending = recent.pendingChunks[0]!;
    const stale = {
      ...recent,
      pendingChunks: [{ ...pending, suites: ["soak", "framework-checkpoint-restore"] }],
    };

    // Manifest without suites: nothing to go on, defer to the explicit "all" load.
    expect(suiteNeedsPendingChunks(recent, "framework-checkpoint-restore", 90)).toBe(false);
    // Suite with no loaded records but present in a pending chunk.
    expect(suiteNeedsPendingChunks(stale, "soak", 90)).toBe(true);
    expect(suiteNeedsPendingChunks(stale, "soak", null)).toBe(true);
    // Loaded suite whose 90-day window (from 2026-09-09) ends long after 2026-03-01.
    expect(suiteNeedsPendingChunks(stale, "framework-checkpoint-restore", 90)).toBe(false);
    // The same suite with a window wide enough to reach the March chunk.
    expect(suiteNeedsPendingChunks(stale, "framework-checkpoint-restore", 365)).toBe(true);
    // A suite the pending chunks do not list.
    expect(suiteNeedsPendingChunks(stale, "other", null)).toBe(false);
  });
});

describe("generic discovery and filtering", () => {
  it("discovers suites, cases, and arbitrary measurements from result data", () => {
    const records = [
      result(),
      result({
        suite: "storage-throughput",
        caseName: "azure-files",
        metric: "copy.throughput",
        displayName: "Copy throughput",
        unit: "bytes_per_second",
      }),
    ];

    const framework = discoverDimensions(records, "framework-checkpoint-restore");
    const storage = discoverDimensions(records, "storage-throughput");

    expect(framework.suites).toEqual([
      "framework-checkpoint-restore",
      "storage-throughput",
    ]);
    expect(framework.cases).toEqual(["vllm"]);
    expect(storage.metrics).toEqual([
      {
        name: "copy.throughput",
        displayName: "Copy throughput",
        unit: "bytes_per_second",
      },
    ]);
  });

  it("filters by date, GPU, outcome, channel, and selected cases", () => {
    const latest = result({ startedAt: "2026-09-09T01:00:00.000Z" });
    const old = result({
      runId: "2",
      caseName: "sglang",
      startedAt: "2026-05-01T01:00:00.000Z",
    });

    expect(
      filterRecords([latest, old], {
        suite: "framework-checkpoint-restore",
        cases: new Set(["vllm"]),
        days: 90,
        gpu: "NVIDIA A100-SXM4-80GB",
        outcome: "passed",
        channel: "schedule",
        referenceTime: Date.parse(latest.startedAt),
      }),
    ).toEqual([latest]);
    expect(
      filterRecords([latest], {
        suite: "framework-checkpoint-restore",
        cases: new Set(),
        days: null,
        gpu: "all",
        outcome: "all",
        channel: "all",
      }),
    ).toEqual([]);
  });

  it("formats known units and humanizes unknown ones", () => {
    expect(formatValue(12.345, "seconds")).toBe("12.35 s");
    expect(formatValue(1073741824, "bytes_per_second")).toBe("1.00 GiB/s");
    expect(formatValue(250, "milliseconds")).toBe("250 ms");
    expect(formatValue(3, "count")).toBe("3");
    expect(formatValue(42.25, "percent")).toBe("42.3 %");
    expect(formatValue(7, "tokens_per_second")).toBe("7.00 tokens per second");
    expect(formatValue(null, "seconds")).toBe("—");
  });

  it("does not resolve prototype members as unit formatters", () => {
    expect(formatValue(1, "toString")).toBe("1.00 toString");
    expect(formatValue(1, "constructor")).toBe("1.00 constructor");
    expect(formatValue(1, "hasOwnProperty")).toBe("1.00 hasOwnProperty");
  });

  it("assigns fallback colors deterministically per case", () => {
    expect(caseColorIndex("azure-files", 4)).toBe(caseColorIndex("azure-files", 4));
    expect(caseColorIndex("azure-files", 4)).toBeLessThan(4);
  });
});

describe("comparison keys", () => {
  it("derives the same key as the history writer", () => {
    const shared = validateResult(SHARED_COMPARISON_KEY.result, new Set([1]));

    expect(deriveComparisonKey(shared)).toBe(SHARED_COMPARISON_KEY.comparisonKey);
  });

  it("prefers the key emitted by the writer over local derivation", () => {
    const record = result();
    const parsed = parseChunk(
      JSON.stringify({ rawPath: "raw.json", comparisonKey: "writer-key", result: record }),
    );

    expect(comparisonKey(parsed.records[0]!)).toBe("writer-key");
    expect(comparisonKey(record)).toBe(deriveComparisonKey(record));
  });

  it("matches the writer's null handling for storage class and custom dimensions", () => {
    const explicitNulls = result();
    explicitNulls.environment = {
      ...explicitNulls.environment,
      storageClass: "from-environment",
      storage: { ...(explicitNulls.environment.storage as object), storageClass: null },
      comparisonDimensions: null,
      sourceGpus: [{ model: 100 }],
      restoreGpus: [{ model: "" }, { model: "NVIDIA A100-SXM4-80GB" }],
    };

    const key = JSON.parse(deriveComparisonKey(explicitNulls)) as Record<string, unknown>;

    expect((key.storage as Record<string, unknown>).storageClass).toBeNull();
    expect(key.custom).toBeNull();
    expect(key.sourceGpuModels).toEqual(["100"]);
    expect(key.restoreGpuModels).toEqual(["NVIDIA A100-SXM4-80GB"]);
  });
});

describe("comparisons and chart points", () => {
  it("uses the previous compatible result and median of the previous seven", () => {
    const history = Array.from({ length: 8 }, (_, index) =>
      result({
        runId: String(index + 1),
        value: index + 1,
        startedAt: `2026-08-${String(index + 1).padStart(2, "0")}T01:00:00.000Z`,
      }),
    );
    const current = result({
      runId: "9",
      value: 20,
      startedAt: "2026-08-09T01:00:00.000Z",
    });

    const comparison = comparableStats(current, "checkpoint.duration", history);

    expect(comparison.previous).not.toBeNull();
    expect(comparison.previous!.value).toBe(8);
    expect(comparison.previous!.deltaPercent).toBe(150);
    expect(comparison.median7).toMatchObject({
      value: 5,
      deltaPercent: 300,
      sampleSize: 7,
    });
  });

  it("only looks backwards in time and ignores failed or incomplete baselines", () => {
    const older = result({ runId: "1", value: 10, startedAt: "2026-08-01T01:00:00.000Z" });
    const failed = result({
      runId: "2",
      outcome: "failed",
      value: 99,
      startedAt: "2026-08-02T01:00:00.000Z",
    });
    const incomplete = result({ runId: "3", value: null, startedAt: "2026-08-03T01:00:00.000Z" });
    const current = result({ runId: "4", value: 12, startedAt: "2026-08-04T01:00:00.000Z" });
    const newer = result({ runId: "5", value: 50, startedAt: "2026-08-05T01:00:00.000Z" });

    const comparison = comparableStats(current, "checkpoint.duration", [
      newer,
      current,
      incomplete,
      failed,
      older,
    ]);

    expect(comparison.previous).toMatchObject({ value: 10, deltaPercent: 20 });
    expect(comparison.median7).toMatchObject({ value: 10, sampleSize: 1 });
  });

  it("does not compare the total duration of a run that did not pass", () => {
    const total = { metric: TEST_TOTAL_METRIC, displayName: "Full E2E test" } as const;
    const history = [
      result({ ...total, runId: "1", value: 300, startedAt: "2026-08-01T01:00:00.000Z" }),
      result({ ...total, runId: "2", value: 310, startedAt: "2026-08-02T01:00:00.000Z" }),
    ];
    // Aborted early: the total is elapsed time to the failure, not a faster run.
    const failed = result({
      ...total,
      runId: "3",
      outcome: "failed",
      value: 45,
      startedAt: "2026-08-03T01:00:00.000Z",
    });

    expect(comparableStats(failed, TEST_TOTAL_METRIC, history)).toEqual({
      previous: null,
      median7: null,
      skippedReason: TOTAL_NOT_COMPARABLE,
    });
    // A phase that genuinely completed on the failed run still compares.
    const phaseHistory = [result({ runId: "1", value: 10, startedAt: "2026-08-01T01:00:00.000Z" })];
    const failedPhase = result({
      runId: "3",
      outcome: "failed",
      value: 12,
      startedAt: "2026-08-03T01:00:00.000Z",
    });
    expect(comparableStats(failedPhase, "checkpoint.duration", phaseHistory).previous).toMatchObject({
      value: 10,
    });
    // A passed run's total compares normally.
    const passed = result({ ...total, runId: "4", value: 290, startedAt: "2026-08-04T01:00:00.000Z" });
    expect(comparableStats(passed, TEST_TOTAL_METRIC, history).previous).toMatchObject({ value: 310 });
  });

  it("does not compare records across relevant environment dimensions", () => {
    const previous = result({ gpuModel: "NVIDIA H100" });
    const current = result({ runId: "2", startedAt: "2026-09-10T01:00:00.000Z" });

    expect(
      comparableStats(current, "checkpoint.duration", [previous]),
    ).toEqual({ previous: null, median7: null });
  });

  it("represents an incomplete failure as a gap instead of zero", () => {
    const failed = result({ outcome: "timed_out", value: null });

    const series = seriesForMetric([failed], "checkpoint.duration")[0]!;
    const point = series.points[0]!;

    expect(point).toMatchObject({ y: null, outcome: "timed_out" });
    expect(point.measurement?.status).toBe("incomplete");
    if (point.measurement?.status !== "incomplete") {
      throw new Error("Expected an incomplete measurement");
    }
    expect(point.measurement.missingReason).toBe("end event not reached");
  });
});

interface ResultOptions {
  suite?: string;
  caseName?: string;
  runId?: string;
  schemaVersion?: number;
  benchmarkVersion?: number;
  startedAt?: string;
  outcome?: Outcome;
  metric?: string;
  displayName?: string;
  unit?: string;
  value?: number | null;
  gpuModel?: string;
}

function result({
  suite = "framework-checkpoint-restore",
  caseName = "vllm",
  runId = "1",
  schemaVersion = 1,
  benchmarkVersion = 1,
  startedAt = "2026-09-09T01:00:00.000Z",
  outcome = "passed",
  metric = "checkpoint.duration",
  displayName = "Checkpoint",
  unit = "seconds",
  value = 10,
  gpuModel = "NVIDIA A100-SXM4-80GB",
}: ResultOptions = {}): BenchmarkResult {
  const measurement: Measurement =
    value == null
      ? {
          name: metric,
          displayName,
          unit,
          value: null,
          status: "incomplete",
          missingReason: "end event not reached",
        }
      : {
          name: metric,
          displayName,
          unit,
          value,
          status: "complete",
        };
  const started = Date.parse(startedAt);
  return {
    schemaVersion,
    benchmarkVersion,
    identity: {
      suite,
      case: caseName,
      test: "test_framework",
      runId,
      runAttempt: 1,
    },
    outcome,
    startedAt,
    finishedAt: new Date((Number.isFinite(started) ? started : 0) + 60_000).toISOString(),
    source: {
      event: "schedule",
      runUrl: `https://github.com/ai-dynamo/snapshot/actions/runs/${runId}`,
      snapshotTag: "v0.0.0-test",
    },
    environment: {
      model: "Qwen/Qwen3-0.6B",
      frameworkImage: "framework@sha256:abc",
      modelCacheMode: "shared-nfs",
      datadogGpuMonitoringMode: "disable",
      sourceGpus: [
        {
          model: gpuModel,
          uuid: "GPU-source",
          driverVersion: "595.58.03",
        },
      ],
      restoreGpus: [
        {
          model: gpuModel,
          uuid: "GPU-restore",
          driverVersion: "595.58.03",
        },
      ],
      storage: {
        storageClass: "azurefile-csi",
        type: "Standard_LRS",
        provisioner: "file.csi.azure.com",
        requestedSize: "64Gi",
        capacity: "64Gi",
        accessModes: ["ReadWriteMany"],
        volumeMode: "Filesystem",
      },
      imagePulls: {
        source: { cacheHit: true },
        restore: { cacheHit: true },
      },
    },
    measurements: [measurement],
    events: [],
    error: outcome === "passed" ? null : { phase: "test", message: "failed" },
  };
}
