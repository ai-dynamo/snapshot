// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export const DEFAULT_METRICS = [
  "checkpoint.duration",
  "restore.to_traffic.duration",
  "test.total.duration",
] as const;

export const VALID_OUTCOMES = [
  "passed",
  "failed",
  "timed_out",
  "skipped",
  "infrastructure_failed",
] as const;

export type Outcome = (typeof VALID_OUTCOMES)[number];

export interface HistoryChunk {
  path: string;
  recordCount: number;
  firstStartedAt: string;
  lastStartedAt: string;
}

export interface HistoryManifest {
  manifestVersion: number;
  historyFormatVersion: number;
  supportedSchemaVersions: number[];
  recordCount: number;
  chunks: HistoryChunk[];
}

export interface BenchmarkIdentity {
  suite: string;
  case: string;
  test: string;
  runId: string;
  runAttempt: number;
}

export interface CompleteMeasurement {
  name: string;
  displayName: string;
  unit: string;
  status: "complete";
  value: number;
}

export interface IncompleteMeasurement {
  name: string;
  displayName: string;
  unit: string;
  status: "incomplete";
  value: null;
  missingReason: string;
}

export type Measurement = CompleteMeasurement | IncompleteMeasurement;
export type DataObject = Record<string, unknown>;

export interface BenchmarkResult {
  schemaVersion: number;
  benchmarkVersion: number;
  identity: BenchmarkIdentity;
  outcome: Outcome;
  startedAt: string;
  finishedAt: string;
  source: DataObject;
  environment: DataObject;
  measurements: Measurement[];
  events?: unknown[];
  error?: unknown;
}

export interface HistoryWarning {
  code: string;
  message: string;
  line?: number;
  path?: string;
}

export interface LoadedHistory {
  manifest: HistoryManifest;
  records: BenchmarkResult[];
  warnings: HistoryWarning[];
}

export interface MetricDefinition {
  name: string;
  displayName: string;
  unit: string;
}

export interface DiscoveredDimensions {
  suites: string[];
  cases: string[];
  metrics: MetricDefinition[];
  gpuModels: string[];
  outcomes: Outcome[];
  channels: string[];
}

export interface RecordFilters {
  suite: string;
  cases?: ReadonlySet<string>;
  days: number | null;
  gpu?: string;
  outcome?: Outcome | "all";
  channel?: string;
  referenceTime?: number;
}

export interface PreviousComparison {
  value: number;
  deltaPercent: number | null;
  startedAt: string;
  runUrl: string | null;
}

export interface MedianComparison {
  value: number;
  deltaPercent: number | null;
  sampleSize: number;
}

export interface MetricComparison {
  previous: PreviousComparison | null;
  median7: MedianComparison | null;
}

export interface MetricPoint {
  x: number;
  y: number | null;
  outcome: Outcome;
  result: BenchmarkResult;
  measurement: Measurement | null;
  comparison: MetricComparison;
}

export interface MetricSeries {
  case: string;
  points: MetricPoint[];
}

export const SUPPORTED_SCHEMA_VERSIONS: ReadonlySet<number> = new Set([1]);

export class DashboardDataError extends Error {
  readonly code: string;

  constructor(message: string, code = "invalid-data") {
    super(message);
    this.name = "DashboardDataError";
    this.code = code;
  }
}

export function parseManifest(value: unknown): HistoryManifest {
  const parsed = typeof value === "string" ? parseJson(value, "manifest") : value;
  requireObject(parsed, "manifest");
  requirePositiveInteger(parsed.manifestVersion, "manifest.manifestVersion");
  if (parsed.manifestVersion !== 1) {
    throw new DashboardDataError(
      `Unsupported manifest version ${parsed.manifestVersion}`,
      "unsupported-manifest",
    );
  }
  requirePositiveInteger(
    parsed.historyFormatVersion,
    "manifest.historyFormatVersion",
  );
  requireNonnegativeInteger(parsed.recordCount, "manifest.recordCount");
  if (!Array.isArray(parsed.supportedSchemaVersions)) {
    throw new DashboardDataError("manifest.supportedSchemaVersions must be an array");
  }
  for (const [index, version] of parsed.supportedSchemaVersions.entries()) {
    requirePositiveInteger(version, `manifest.supportedSchemaVersions[${index}]`);
  }
  if (!Array.isArray(parsed.chunks)) {
    throw new DashboardDataError("manifest.chunks must be an array");
  }

  for (const [index, chunk] of parsed.chunks.entries()) {
    requireObject(chunk, `manifest.chunks[${index}]`);
    if (!isSafeChunkPath(chunk.path)) {
      throw new DashboardDataError(
        `manifest.chunks[${index}].path is not a safe monthly index path`,
      );
    }
    requireNonnegativeInteger(
      chunk.recordCount,
      `manifest.chunks[${index}].recordCount`,
    );
    requireTimestamp(chunk.firstStartedAt, `manifest.chunks[${index}].firstStartedAt`);
    requireTimestamp(chunk.lastStartedAt, `manifest.chunks[${index}].lastStartedAt`);
  }
  return parsed as unknown as HistoryManifest;
}

export function parseChunk(
  text: string,
  supportedVersions: ReadonlySet<number> = SUPPORTED_SCHEMA_VERSIONS,
): { records: BenchmarkResult[]; warnings: HistoryWarning[] } {
  const records: BenchmarkResult[] = [];
  const warnings: HistoryWarning[] = [];
  const lines = text.split(/\r?\n/).filter((line) => line.trim() !== "");
  for (const [index, line] of lines.entries()) {
    try {
      const entry = parseJson(line, `chunk line ${index + 1}`);
      requireObject(entry, `chunk line ${index + 1}`);
      const result = validateResult(entry.result, supportedVersions);
      records.push(result);
    } catch (error: unknown) {
      const issue =
        error instanceof DashboardDataError
          ? error
          : new DashboardDataError(String(error));
      warnings.push({ code: issue.code, message: issue.message, line: index + 1 });
    }
  }
  return { records, warnings };
}

export function validateResult(
  value: unknown,
  supportedVersions: ReadonlySet<number> = SUPPORTED_SCHEMA_VERSIONS,
): BenchmarkResult {
  requireObject(value, "result");
  requirePositiveInteger(value.schemaVersion, "result.schemaVersion");
  if (!supportedVersions.has(value.schemaVersion)) {
    throw new DashboardDataError(
      `Unsupported result schema version ${value.schemaVersion}`,
      "unsupported-schema",
    );
  }
  requirePositiveInteger(value.benchmarkVersion, "result.benchmarkVersion");
  requireObject(value.identity, "result.identity");
  for (const field of ["suite", "case", "test", "runId"] as const) {
    requireString(value.identity[field], `result.identity.${field}`);
  }
  requirePositiveInteger(value.identity.runAttempt, "result.identity.runAttempt");
  if (!isOutcome(value.outcome)) {
    throw new DashboardDataError(`Unknown result outcome ${String(value.outcome)}`);
  }
  requireTimestamp(value.startedAt, "result.startedAt");
  requireTimestamp(value.finishedAt, "result.finishedAt");
  requireObject(value.source, "result.source");
  requireObject(value.environment, "result.environment");
  if (!Array.isArray(value.measurements) || value.measurements.length === 0) {
    throw new DashboardDataError("result.measurements must be a non-empty array");
  }

  const names = new Set<string>();
  for (const [index, item] of value.measurements.entries()) {
    const location = `result.measurements[${index}]`;
    requireObject(item, location);
    requireString(item.name, `${location}.name`);
    requireString(item.displayName, `${location}.displayName`);
    requireString(item.unit, `${location}.unit`);
    if (names.has(item.name)) {
      throw new DashboardDataError(`Duplicate measurement ${item.name}`);
    }
    names.add(item.name);
    if (item.status === "complete") {
      if (typeof item.value !== "number" || !Number.isFinite(item.value)) {
        throw new DashboardDataError(`${location}.value must be finite`);
      }
    } else if (item.status === "incomplete") {
      if (item.value !== null) {
        throw new DashboardDataError(`${location}.value must be null when incomplete`);
      }
      requireString(item.missingReason, `${location}.missingReason`);
    } else {
      throw new DashboardDataError(`${location}.status is invalid`);
    }
  }
  return value as unknown as BenchmarkResult;
}

export async function loadHistory(
  siteRoot: string | URL,
  fetchImpl: typeof fetch = globalThis.fetch,
): Promise<LoadedHistory> {
  const root = new URL(siteRoot, globalThis.location?.href ?? "http://localhost/");
  const manifestResponse = await fetchImpl(new URL("index/manifest.json", root));
  if (!manifestResponse.ok) {
    throw new DashboardDataError(
      `Could not load benchmark manifest (${manifestResponse.status})`,
      "network",
    );
  }
  const manifest = parseManifest(await manifestResponse.text());
  const records: BenchmarkResult[] = [];
  const warnings: HistoryWarning[] = [];
  for (const chunk of manifest.chunks) {
    const response = await fetchImpl(new URL(chunk.path, root));
    if (!response.ok) {
      warnings.push({
        code: "network",
        message: `Could not load ${chunk.path} (${response.status})`,
      });
      continue;
    }
    const parsed = parseChunk(await response.text());
    records.push(...parsed.records);
    warnings.push(...parsed.warnings.map((warning) => ({ ...warning, path: chunk.path })));
  }

  const unique = new Map<string, BenchmarkResult>();
  for (const result of records) {
    const key = resultIdentity(result);
    if (!unique.has(key)) {
      unique.set(key, result);
    } else {
      warnings.push({ code: "duplicate", message: `Duplicate result identity ${key}` });
    }
  }
  return {
    manifest,
    records: [...unique.values()].sort(compareResults),
    warnings,
  };
}

export function discoverDimensions(
  records: readonly BenchmarkResult[],
  suite: string,
): DiscoveredDimensions {
  const suiteRecords = records.filter((result) => result.identity.suite === suite);
  const metrics = new Map<string, MetricDefinition>();
  for (const result of suiteRecords) {
    for (const item of result.measurements) {
      if (!metrics.has(item.name)) {
        metrics.set(item.name, {
          name: item.name,
          displayName: item.displayName,
          unit: item.unit,
        });
      }
    }
  }
  return {
    suites: sortedUnique(records.map((result) => result.identity.suite)),
    cases: sortedUnique(suiteRecords.map((result) => result.identity.case)),
    metrics: [...metrics.values()].sort((left, right) => {
      const leftDefault = DEFAULT_METRICS.indexOf(
        left.name as (typeof DEFAULT_METRICS)[number],
      );
      const rightDefault = DEFAULT_METRICS.indexOf(
        right.name as (typeof DEFAULT_METRICS)[number],
      );
      if (leftDefault >= 0 || rightDefault >= 0) {
        if (leftDefault < 0) return 1;
        if (rightDefault < 0) return -1;
        return leftDefault - rightDefault;
      }
      return left.displayName.localeCompare(right.displayName);
    }),
    gpuModels: sortedUnique(suiteRecords.flatMap((result) => gpuModels(result))),
    outcomes: VALID_OUTCOMES.filter((outcome) =>
      suiteRecords.some((result) => result.outcome === outcome),
    ),
    channels: sortedUnique(suiteRecords.map(runChannel)),
  };
}

export function filterRecords(
  records: readonly BenchmarkResult[],
  filters: RecordFilters,
): BenchmarkResult[] {
  const referenceTime =
    filters.referenceTime ??
    Math.max(...records.map((result) => Date.parse(result.startedAt)), 0);
  const cutoff =
    filters.days == null ? null : referenceTime - filters.days * 24 * 60 * 60 * 1000;
  return records.filter((result) => {
    if (result.identity.suite !== filters.suite) return false;
    if (filters.cases && !filters.cases.has(result.identity.case)) return false;
    if (cutoff != null && Date.parse(result.startedAt) < cutoff) return false;
    if (filters.gpu && filters.gpu !== "all" && !gpuModels(result).includes(filters.gpu)) {
      return false;
    }
    if (filters.outcome && filters.outcome !== "all" && result.outcome !== filters.outcome) {
      return false;
    }
    if (filters.channel && filters.channel !== "all" && runChannel(result) !== filters.channel) {
      return false;
    }
    return true;
  });
}

export function seriesForMetric(
  records: readonly BenchmarkResult[],
  metricName: string,
  comparisonHistory: readonly BenchmarkResult[] = records,
): MetricSeries[] {
  const cases = sortedUnique(records.map((result) => result.identity.case));
  return cases.map((caseName) => ({
    case: caseName,
    points: records
      .filter((result) => result.identity.case === caseName)
      .sort(compareResults)
      .map((result) => {
        const item = measurement(result, metricName);
        return {
          x: Date.parse(result.startedAt),
          y: item?.status === "complete" ? item.value : null,
          outcome: result.outcome,
          result,
          measurement: item,
          comparison: comparableStats(result, metricName, comparisonHistory),
        };
      }),
  }));
}

export function comparableStats(
  current: BenchmarkResult,
  metricName: string,
  history: readonly BenchmarkResult[],
): MetricComparison {
  const currentMeasurement = measurement(current, metricName);
  if (!currentMeasurement || currentMeasurement.status !== "complete") {
    return { previous: null, median7: null };
  }
  const dimensions = comparisonDimensions(current);
  const candidates = history
    .filter(
      (result) =>
        result.outcome === "passed" &&
        resultIdentity(result) !== resultIdentity(current) &&
        compareResults(result, current) < 0 &&
        comparisonDimensions(result) === dimensions,
    )
    .map((result) => ({ result, item: measurement(result, metricName) }))
    .filter(
      (candidate): candidate is { result: BenchmarkResult; item: CompleteMeasurement } =>
        candidate.item?.status === "complete" &&
        candidate.item.unit === currentMeasurement.unit,
    )
    .sort((left, right) => compareResults(right.result, left.result));

  const previous = candidates[0];
  if (!previous) {
    return { previous: null, median7: null };
  }
  const values = candidates
    .slice(0, 7)
    .map(({ item }) => item.value)
    .sort((left, right) => left - right);
  const middle = Math.floor(values.length / 2);
  const median =
    values.length % 2 === 0
      ? (values[middle - 1]! + values[middle]!) / 2
      : values[middle]!;
  return {
    previous: {
      value: previous.item.value,
      deltaPercent: deltaPercent(currentMeasurement.value, previous.item.value),
      startedAt: previous.result.startedAt,
      runUrl: stringProperty(previous.result.source, "runUrl"),
    },
    median7: {
      value: median,
      deltaPercent: deltaPercent(currentMeasurement.value, median),
      sampleSize: values.length,
    },
  };
}

export function measurement(result: BenchmarkResult, name: string): Measurement | null {
  return result.measurements.find((item) => item.name === name) ?? null;
}

export function gpuModels(result: BenchmarkResult): string[] {
  const environment = result.environment;
  const generic = modelsFrom(environment.gpus);
  const source = modelsFrom(environment.sourceGpus ?? environment.gpus);
  const restore = modelsFrom(environment.restoreGpus ?? environment.gpus);
  return sortedUnique([...source, ...restore, ...(source.length || restore.length ? [] : generic)]);
}

export function runChannel(result: BenchmarkResult): string {
  return stringProperty(result.source, "event") ?? "unknown";
}

export function stringProperty(value: unknown, key: string): string | null {
  const object = objectOrEmpty(value);
  const property = object[key];
  return typeof property === "string" && property.trim() !== "" ? property : null;
}

export function formatValue(value: number | null, unit: string): string {
  if (value == null) return "—";
  if (unit === "seconds") return `${value.toFixed(2)} s`;
  if (unit === "bytes") return formatBytes(value);
  return `${value.toFixed(2)} ${unit}`;
}

export function formatDelta(value: number | null): string {
  if (value == null) return "—";
  return `${value >= 0 ? "+" : ""}${value.toFixed(1)}%`;
}

export function safeLink(value: unknown): string | null {
  if (typeof value !== "string") return null;
  try {
    const parsed = new URL(value);
    return ["https:", "http:"].includes(parsed.protocol) ? parsed.href : null;
  } catch {
    return null;
  }
}

function comparisonDimensions(result: BenchmarkResult): string {
  const environment = result.environment;
  const storage = objectOrEmpty(environment.storage);
  const imagePulls = objectOrEmpty(environment.imagePulls);
  const genericGpus = modelsFrom(environment.gpus);
  const sourceGpus = modelsFrom(environment.sourceGpus);
  const restoreGpus = modelsFrom(environment.restoreGpus);
  return canonicalJson({
    schemaVersion: result.schemaVersion,
    benchmarkVersion: result.benchmarkVersion,
    suite: result.identity.suite,
    case: result.identity.case,
    test: result.identity.test,
    sourceGpuModels: sourceGpus.length ? sourceGpus : genericGpus,
    restoreGpuModels: restoreGpus.length ? restoreGpus : genericGpus,
    frameworkImage: environment.frameworkImage ?? null,
    model: environment.model ?? null,
    storage: {
      storageClass: storage.storageClass ?? environment.storageClass ?? null,
      type: storage.type ?? null,
      provisioner: storage.provisioner ?? null,
      requestedSize: storage.requestedSize ?? null,
      capacity: storage.capacity ?? null,
      accessModes: Array.isArray(storage.accessModes)
        ? [...storage.accessModes].sort()
        : [],
      volumeMode: storage.volumeMode ?? null,
    },
    modelCacheMode: environment.modelCacheMode ?? null,
    imageCache: {
      source: cacheHit(imagePulls.source),
      restore: cacheHit(imagePulls.restore),
    },
    datadogGpuMonitoringMode: environment.datadogGpuMonitoringMode ?? null,
    custom: objectOrEmpty(environment.comparisonDimensions),
  });
}

function compareResults(left: BenchmarkResult, right: BenchmarkResult): number {
  return resultSortKey(left).localeCompare(resultSortKey(right));
}

function resultSortKey(result: BenchmarkResult): string {
  const identity = result.identity;
  return [
    result.startedAt,
    identity.runId,
    String(identity.runAttempt).padStart(8, "0"),
    identity.suite,
    identity.case,
    identity.test,
  ].join("\u0000");
}

function resultIdentity(result: BenchmarkResult): string {
  const identity = result.identity;
  return [
    identity.suite,
    identity.case,
    identity.test,
    identity.runId,
    identity.runAttempt,
  ].join("\u0000");
}

function deltaPercent(current: number, baseline: number): number | null {
  if (baseline === 0) return null;
  return ((current - baseline) / baseline) * 100;
}

function modelsFrom(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return sortedUnique(
    value.flatMap((item) => {
      const model = stringProperty(item, "model");
      return model ? [model] : [];
    }),
  );
}

function cacheHit(value: unknown): boolean | null {
  const object = objectOrEmpty(value);
  return typeof object.cacheHit === "boolean" ? object.cacheHit : null;
}

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (isObject(value)) {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value) ?? "null";
}

function sortedUnique(values: readonly string[]): string[] {
  return [...new Set(values)].sort((left, right) => left.localeCompare(right));
}

function objectOrEmpty(value: unknown): DataObject {
  return isObject(value) ? value : {};
}

function parseJson(value: string, location: string): unknown {
  try {
    return JSON.parse(value) as unknown;
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    throw new DashboardDataError(`${location} is not valid JSON: ${message}`);
  }
}

function isSafeChunkPath(value: unknown): value is string {
  return (
    typeof value === "string" &&
    /^index\/v[1-9][0-9]*\/[0-9]{4}-(0[1-9]|1[0-2])\.ndjson$/.test(value)
  );
}

function isOutcome(value: unknown): value is Outcome {
  return typeof value === "string" && VALID_OUTCOMES.some((item) => item === value);
}

function isObject(value: unknown): value is DataObject {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function requireObject(value: unknown, location: string): asserts value is DataObject {
  if (!isObject(value)) {
    throw new DashboardDataError(`${location} must be an object`);
  }
}

function requireString(value: unknown, location: string): asserts value is string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new DashboardDataError(`${location} must be a non-empty string`);
  }
}

function requireTimestamp(value: unknown, location: string): asserts value is string {
  requireString(value, location);
  if (!Number.isFinite(Date.parse(value))) {
    throw new DashboardDataError(`${location} must be an RFC3339 timestamp`);
  }
}

function requirePositiveInteger(value: unknown, location: string): asserts value is number {
  if (!Number.isInteger(value) || Number(value) < 1) {
    throw new DashboardDataError(`${location} must be a positive integer`);
  }
}

function requireNonnegativeInteger(
  value: unknown,
  location: string,
): asserts value is number {
  if (!Number.isInteger(value) || Number(value) < 0) {
    throw new DashboardDataError(`${location} must be a non-negative integer`);
  }
}

function formatBytes(value: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"] as const;
  let current = value;
  for (const unit of units) {
    if (Math.abs(current) < 1024 || unit === units.at(-1)) {
      return `${current.toFixed(2)} ${unit}`;
    }
    current /= 1024;
  }
  return `${value.toFixed(0)} B`;
}
