// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import {
  DashboardDataError,
  comparableStats,
  discoverDimensions,
  filterRecords,
  parseChunk,
  parseManifest,
  seriesForMetric,
} from "./data.ts";
import type { BenchmarkResult, Measurement, Outcome } from "./data.ts";

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
  benchmarkVersion = 2,
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
    finishedAt: new Date(Date.parse(startedAt) + 60_000).toISOString(),
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
