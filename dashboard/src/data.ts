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

export const DEFAULT_RECENT_DAYS = 90;
export const STALE_HISTORY_DAYS = 3;
const DAY_MILLISECONDS = 24 * 60 * 60 * 1000;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

export type Outcome = (typeof VALID_OUTCOMES)[number];

export interface HistoryChunk {
  path: string;
  month?: string;
  recordCount: number;
  firstStartedAt: string;
  lastStartedAt: string;
}

export interface HistoryManifest {
  manifestVersion: number;
  historyFormatVersion: number;
  supportedSchemaVersions: number[];
  recordCount: number;
  newestResultAt?: string | null;
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
  siteRoot: string;
  manifest: HistoryManifest;
  records: BenchmarkResult[];
  warnings: HistoryWarning[];
  pendingChunks: HistoryChunk[];
}

export interface LoadOptions {
  recentDays?: number | null;
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

const emittedComparisonKeys = new WeakMap<BenchmarkResult, string>();
const comparisonKeys = new WeakMap<BenchmarkResult, string>();
const comparisonIndexes = new WeakMap<
  readonly BenchmarkResult[],
  Map<string, BenchmarkResult[]>
>();

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
  if (parsed.newestResultAt != null) {
    requireTimestamp(parsed.newestResultAt, "manifest.newestResultAt");
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
      if (typeof entry.comparisonKey === "string" && entry.comparisonKey !== "") {
        emittedComparisonKeys.set(result, entry.comparisonKey);
      }
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
  options: LoadOptions = {},
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
  const recentDays =
    options.recentDays === undefined ? DEFAULT_RECENT_DAYS : options.recentDays;
  const newest = Math.max(
    ...manifest.chunks.map((chunk) => Date.parse(chunk.lastStartedAt)),
    Number.NEGATIVE_INFINITY,
  );
  const cutoff =
    recentDays == null || !Number.isFinite(newest)
      ? null
      : newest - recentDays * DAY_MILLISECONDS;
  const eager = manifest.chunks.filter(
    (chunk) => cutoff == null || Date.parse(chunk.lastStartedAt) >= cutoff,
  );
  const pending = manifest.chunks.filter((chunk) => !eager.includes(chunk));
  const loaded = await fetchChunks(root, eager, fetchImpl);
  return assembleHistory(root.href, manifest, loaded.records, loaded.warnings, pending);
}

export async function loadRemainingHistory(
  history: LoadedHistory,
  fetchImpl: typeof fetch = globalThis.fetch,
): Promise<LoadedHistory> {
  if (history.pendingChunks.length === 0) return history;
  const loaded = await fetchChunks(new URL(history.siteRoot), history.pendingChunks, fetchImpl);
  return assembleHistory(
    history.siteRoot,
    history.manifest,
    [...history.records, ...loaded.records],
    [...history.warnings, ...loaded.warnings],
    [],
  );
}

async function fetchChunks(
  root: URL,
  chunks: readonly HistoryChunk[],
  fetchImpl: typeof fetch,
): Promise<{ records: BenchmarkResult[]; warnings: HistoryWarning[] }> {
  const results = await Promise.all(
    chunks.map(async (chunk) => {
      const response = await fetchImpl(new URL(chunk.path, root));
      if (!response.ok) {
        return {
          records: [] as BenchmarkResult[],
          warnings: [
            {
              code: "network",
              message: `Could not load ${chunk.path} (${response.status})`,
              path: chunk.path,
            },
          ],
        };
      }
      const parsed = parseChunk(await response.text());
      return {
        records: parsed.records,
        warnings: parsed.warnings.map((warning) => ({ ...warning, path: chunk.path })),
      };
    }),
  );
  return {
    records: results.flatMap((item) => item.records),
    warnings: results.flatMap((item) => item.warnings),
  };
}

function assembleHistory(
  siteRoot: string,
  manifest: HistoryManifest,
  records: readonly BenchmarkResult[],
  warnings: readonly HistoryWarning[],
  pendingChunks: HistoryChunk[],
): LoadedHistory {
  const unique = new Map<string, BenchmarkResult>();
  const duplicates: HistoryWarning[] = [];
  for (const result of records) {
    const key = resultIdentity(result);
    if (!unique.has(key)) {
      unique.set(key, result);
    } else {
      duplicates.push({ code: "duplicate", message: `Duplicate result identity ${key}` });
    }
  }
  return {
    siteRoot,
    manifest,
    records: [...unique.values()].sort(compareResults),
    warnings: [...warnings, ...duplicates],
    pendingChunks,
  };
}

export function newestResultAt(history: LoadedHistory): string | null {
  let newest: string | null = null;
  for (const result of history.records) {
    if (newest == null || Date.parse(result.finishedAt) > Date.parse(newest)) {
      newest = result.finishedAt;
    }
  }
  if (newest == null && history.manifest.newestResultAt) {
    return history.manifest.newestResultAt;
  }
  return newest;
}

export function daysSince(timestamp: string, now: number = Date.now()): number {
  return Math.max(0, Math.floor((now - Date.parse(timestamp)) / DAY_MILLISECONDS));
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
    filters.days == null ? null : referenceTime - filters.days * DAY_MILLISECONDS;
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
  const bucket = comparisonIndex(history).get(comparisonKey(current)) ?? [];
  const currentIdentity = resultIdentity(current);
  const candidates: Array<{ result: BenchmarkResult; item: CompleteMeasurement }> = [];
  for (let index = lowerBound(bucket, current) - 1; index >= 0; index -= 1) {
    if (candidates.length === 7) break;
    const result = bucket[index]!;
    if (resultIdentity(result) === currentIdentity) continue;
    const item = measurement(result, metricName);
    if (item?.status === "complete" && item.unit === currentMeasurement.unit) {
      candidates.push({ result, item });
    }
  }

  const previous = candidates[0];
  if (!previous) {
    return { previous: null, median7: null };
  }
  const values = candidates.map(({ item }) => item.value).sort((left, right) => left - right);
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

export function measurementComparisons(
  result: BenchmarkResult,
  history: readonly BenchmarkResult[],
): Array<{ measurement: Measurement; comparison: MetricComparison }> {
  return result.measurements.map((item) => ({
    measurement: item,
    comparison: comparableStats(result, item.name, history),
  }));
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

export function commitUrl(result: BenchmarkResult): string | null {
  const commit = stringProperty(result.source, "commit");
  const runUrl = safeLink(result.source.runUrl);
  if (!commit || !runUrl || !/^[0-9a-fA-F]{7,64}$/.test(commit)) return null;
  const marker = runUrl.indexOf("/actions/runs/");
  if (marker < 0) return null;
  return `${runUrl.slice(0, marker)}/commit/${commit}`;
}

export function shortCommit(result: BenchmarkResult): string | null {
  const commit = stringProperty(result.source, "commit");
  return commit ? commit.slice(0, 8) : null;
}

const UNIT_FORMATS: Readonly<Record<string, (value: number) => string>> = {
  seconds: (value) => `${value.toFixed(2)} s`,
  milliseconds: (value) => `${value.toFixed(0)} ms`,
  bytes: formatBytes,
  bytes_per_second: (value) => `${formatBytes(value)}/s`,
  count: (value) => value.toLocaleString(),
  percent: (value) => `${value.toFixed(1)} %`,
};

export function formatValue(value: number | null, unit: string): string {
  if (value == null) return "—";
  const format = UNIT_FORMATS[unit];
  if (format) return format(value);
  return `${value.toFixed(2)} ${humanizeUnit(unit)}`;
}

export function humanizeUnit(unit: string): string {
  return unit.replaceAll("_", " ");
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

export function caseColorIndex(caseName: string, paletteSize: number): number {
  let hash = 0;
  for (const character of caseName) {
    hash = (hash * 31 + (character.codePointAt(0) ?? 0)) >>> 0;
  }
  return hash % paletteSize;
}

export function comparisonKey(result: BenchmarkResult): string {
  let key = comparisonKeys.get(result);
  if (key === undefined) {
    key = emittedComparisonKeys.get(result) ?? deriveComparisonKey(result);
    comparisonKeys.set(result, key);
  }
  return key;
}

export function deriveComparisonKey(result: BenchmarkResult): string {
  const environment = result.environment;
  const storage = objectOrEmpty(environment.storage);
  const imagePulls = objectOrEmpty(environment.imagePulls);
  const genericGpus = modelsFrom(environment.gpus);
  const sourceGpus = modelsFrom(environment.sourceGpus);
  const restoreGpus = modelsFrom(environment.restoreGpus);
  const accessModes = storage.accessModes;
  return canonicalJson({
    schemaVersion: result.schemaVersion,
    benchmarkVersion: result.benchmarkVersion,
    suite: result.identity.suite,
    case: result.identity.case,
    test: result.identity.test,
    sourceGpuModels: sourceGpus.length ? sourceGpus : genericGpus,
    restoreGpuModels: restoreGpus.length ? restoreGpus : genericGpus,
    frameworkImage:
      pythonTruthy(environment.frameworkImageDigest)
        ? environment.frameworkImageDigest
        : nullable(environment.frameworkImage),
    model: nullable(environment.model),
    storage: {
      storageClass:
        "storageClass" in storage
          ? nullable(storage.storageClass)
          : nullable(environment.storageClass),
      type: nullable(storage.type),
      provisioner: nullable(storage.provisioner),
      requestedSize: nullable(storage.requestedSize),
      capacity: nullable(storage.capacity),
      accessModes: Array.isArray(accessModes) ? [...accessModes].sort(compareUnknown) : [],
      volumeMode: nullable(storage.volumeMode),
    },
    modelCacheMode: nullable(environment.modelCacheMode),
    imageCache: {
      source: cacheHit(imagePulls.source),
      restore: cacheHit(imagePulls.restore),
    },
    datadogGpuMonitoringMode: nullable(environment.datadogGpuMonitoringMode),
    custom:
      "comparisonDimensions" in environment
        ? nullable(environment.comparisonDimensions)
        : {},
  });
}

function comparisonIndex(
  history: readonly BenchmarkResult[],
): Map<string, BenchmarkResult[]> {
  let index = comparisonIndexes.get(history);
  if (!index) {
    index = new Map();
    for (const result of history) {
      if (result.outcome !== "passed") continue;
      const key = comparisonKey(result);
      let bucket = index.get(key);
      if (!bucket) {
        bucket = [];
        index.set(key, bucket);
      }
      bucket.push(result);
    }
    for (const bucket of index.values()) bucket.sort(compareResults);
    comparisonIndexes.set(history, index);
  }
  return index;
}

function lowerBound(sorted: readonly BenchmarkResult[], target: BenchmarkResult): number {
  let low = 0;
  let high = sorted.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if (compareResults(sorted[middle]!, target) < 0) {
      low = middle + 1;
    } else {
      high = middle;
    }
  }
  return low;
}

export function compareResults(left: BenchmarkResult, right: BenchmarkResult): number {
  const byTime = Date.parse(left.startedAt) - Date.parse(right.startedAt);
  if (byTime !== 0) return byTime;
  return (
    compareStrings(left.identity.runId, right.identity.runId) ||
    left.identity.runAttempt - right.identity.runAttempt ||
    compareStrings(left.identity.suite, right.identity.suite) ||
    compareStrings(left.identity.case, right.identity.case) ||
    compareStrings(left.identity.test, right.identity.test)
  );
}

function resultIdentity(result: BenchmarkResult): string {
  const identity = result.identity;
  return [
    identity.suite,
    identity.case,
    identity.test,
    identity.runId,
    identity.runAttempt,
  ].join(" ");
}

function deltaPercent(current: number, baseline: number): number | null {
  if (baseline === 0) return null;
  return ((current - baseline) / baseline) * 100;
}

function modelsFrom(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  const models = new Set<string>();
  for (const item of value) {
    if (isObject(item) && pythonTruthy(item.model)) {
      models.add(String(item.model));
    }
  }
  return [...models].sort(compareStrings);
}

function cacheHit(value: unknown): boolean | null {
  const object = objectOrEmpty(value);
  return typeof object.cacheHit === "boolean" ? object.cacheHit : null;
}

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (isObject(value)) {
    return `{${Object.keys(value)
      .sort(compareStrings)
      .map((key) => `${jsonString(key)}:${canonicalJson(value[key])}`)
      .join(",")}}`;
  }
  if (typeof value === "string") return jsonString(value);
  return JSON.stringify(value) ?? "null";
}

function jsonString(value: string): string {
  return JSON.stringify(value).replace(
    /[\u0080-\uffff]/g,
    (character) => `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`,
  );
}

function pythonTruthy(value: unknown): boolean {
  if (value == null || value === false || value === 0 || value === "") return false;
  if (Array.isArray(value)) return value.length > 0;
  if (isObject(value)) return Object.keys(value).length > 0;
  return true;
}

function nullable(value: unknown): unknown {
  return value === undefined ? null : value;
}

function compareStrings(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function compareUnknown(left: unknown, right: unknown): number {
  return compareStrings(String(left), String(right));
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
  if (!RFC3339.test(value) || !Number.isFinite(Date.parse(value))) {
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
