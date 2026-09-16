// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
  Chart,
  registerables,
  type ChartDataset,
  type ChartOptions,
  type ScriptableContext,
  type TooltipItem,
} from "chart.js";

import {
  DEFAULT_METRICS,
  STALE_HISTORY_DAYS,
  VALID_OUTCOMES,
  caseColorIndex,
  commitUrl,
  daysSince,
  discoverDimensions,
  filterRecords,
  formatDelta,
  formatValue,
  gpuModels,
  humanizeUnit,
  loadHistory,
  loadRemainingHistory,
  measurement,
  measurementComparisons,
  newestResultAt,
  runChannel,
  safeLink,
  measurementStageSegments,
  recentStageComparison,
  seriesForMetric,
  shortCommit,
  stringProperty,
  suiteNeedsPendingChunks,
} from "./data.ts";
import type {
  BenchmarkResult,
  LoadedHistory,
  MetricComparison,
  MetricDefinition,
  MetricPoint,
  MetricSeries,
  Outcome,
} from "./data.ts";
import { loadPreviewMetadata, type PreviewMetadata } from "./preview.ts";
import "./style.css";

Chart.register(...registerables);

const COLORS: Readonly<Record<string, string>> = {
  vllm: "#76b900",
  sglang: "#5d7fe5",
  "tensorrt-llm": "#ef9f27",
};
const FALLBACK_COLORS = ["#18a999", "#b05fd3", "#e05a47", "#517891"] as const;
const DEFAULT_METRIC_NAMES: ReadonlySet<string> = new Set(DEFAULT_METRICS);
const DEFAULT_RANGE = "90";

interface DashboardPoint extends MetricPoint {
  metric: MetricDefinition;
}

interface CheckboxOption {
  value: string;
  label: string;
  checked: boolean;
}

interface SelectOption {
  value: string;
  label: string;
}

const elements = {
  preview: requiredElement<HTMLElement>("#preview-banner"),
  previewTitle: requiredElement<HTMLElement>("#preview-title"),
  previewDescription: requiredElement<HTMLElement>("#preview-description"),
  previewRunLink: requiredElement<HTMLAnchorElement>("#preview-run-link"),
  status: requiredElement<HTMLElement>("#load-status"),
  suite: requiredElement<HTMLSelectElement>("#suite-filter"),
  suiteField: requiredElement<HTMLElement>("#suite-field"),
  date: requiredElement<HTMLSelectElement>("#date-filter"),
  gpu: requiredElement<HTMLSelectElement>("#gpu-filter"),
  outcome: requiredElement<HTMLSelectElement>("#outcome-filter"),
  channel: requiredElement<HTMLSelectElement>("#channel-filter"),
  cases: requiredElement<HTMLElement>("#case-filters"),
  metrics: requiredElement<HTMLElement>("#metric-filters"),
  reset: requiredElement<HTMLButtonElement>("#reset-filters"),
  count: requiredElement<HTMLElement>("#visible-count"),
  empty: requiredElement<HTMLElement>("#no-results"),
  charts: requiredElement<HTMLElement>("#charts"),
  tableHead: requiredElement<HTMLTableSectionElement>("#latest-head"),
  tableBody: requiredElement<HTMLTableSectionElement>("#latest-body"),
  dialog: requiredElement<HTMLDialogElement>("#run-details"),
  dialogContent: requiredElement<HTMLElement>("#details-content"),
  closeDialog: requiredElement<HTMLButtonElement>("#close-details"),
};

let history: LoadedHistory;
let charts: Chart<"line", DashboardPoint[]>[] = [];
let stageChart: Chart<"bar", number[]> | null = null;
let stageComparisonCharts: Chart<"bar", number[]>[] = [];
let currentMetrics: ReadonlySet<string> = new Set();
const STAGE_COMPARISON_RUN_COUNT = 7;

const STAGE_COLORS = [
  "#76b900",
  "#5d7fe5",
  "#ef9f27",
  "#18a999",
  "#b05fd3",
  "#e05a47",
  "#517891",
  "#c9a227",
  "#8a5cf6",
] as const;

// Colour by measurement name, not by position in the current selection, so a
// stage keeps its colour when other measurements are checked or unchecked.
function stageColor(name: string): string {
  return STAGE_COLORS[caseColorIndex(name, STAGE_COLORS.length)]!;
}

async function start() {
  try {
    const root = new URL("./", document.baseURI);
    const [loadedHistory, preview] = await Promise.all([
      loadHistory(root),
      loadPreviewMetadata(root),
    ]);
    history = loadedHistory;
    renderPreview(preview);
    configureSuites();
    await ensureRangeLoaded();
    configureSuiteFilters();
    bindEvents();
    render();
    renderStatus();
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    elements.status.textContent = `Benchmark history unavailable: ${message}`;
    elements.status.classList.add("status--error");
    elements.empty.hidden = false;
    elements.empty.textContent = "The benchmark data could not be loaded.";
  }
}

function renderStatus() {
  elements.status.classList.remove("status--stale", "status--error");
  const chunkCount = history.manifest.chunks.length;
  const loadedChunks = chunkCount - history.pendingChunks.length;
  const parts = [
    `Loaded ${history.records.length} benchmark result${history.records.length === 1 ? "" : "s"} ` +
      `from ${loadedChunks} of ${chunkCount} monthly index${chunkCount === 1 ? "" : "es"}`,
  ];
  if (history.pendingChunks.length) {
    parts.push(
      `${history.pendingChunks.length} older month${history.pendingChunks.length === 1 ? "" : "s"} load on demand`,
    );
  }
  const newest = newestResultAt(history);
  if (newest) {
    const age = daysSince(newest);
    parts.push(`newest result ${fullDate(newest)} (${age === 0 ? "today" : `${age} day${age === 1 ? "" : "s"} ago`})`);
    if (age > STALE_HISTORY_DAYS) {
      elements.status.classList.add("status--stale");
      parts.push("history looks stale");
    }
  }
  if (history.warnings.length) {
    parts.push(
      `${history.warnings.length} record warning${history.warnings.length === 1 ? "" : "s"}`,
    );
  }
  if (history.loadFailures.length) {
    elements.status.classList.add("status--error");
    parts.push(
      ...history.loadFailures.map((failure) => failure.message),
      "those months stay pending; change the date range or suite to retry",
    );
  }
  elements.status.textContent = `${parts.join(" · ")}.`;
  renderWarnings();
}

function renderWarnings() {
  document.querySelector("#load-warnings")?.remove();
  if (!history.warnings.length) return;
  const details = document.createElement("details");
  details.id = "load-warnings";
  details.className = "warnings";
  const summary = document.createElement("summary");
  summary.textContent = `${history.warnings.length} record warning${history.warnings.length === 1 ? "" : "s"}`;
  const list = document.createElement("ul");
  for (const warning of history.warnings) {
    const location = [warning.path, warning.line].filter((part) => part != null).join(":");
    const text = `${location ? `${location}: ` : ""}${warning.message} (${warning.code})`;
    console.warn(`benchmark history: ${text}`);
    const item = document.createElement("li");
    item.textContent = text;
    list.append(item);
  }
  details.append(summary, list);
  elements.status.after(details);
}

function renderPreview(preview: PreviewMetadata | null): void {
  if (preview === null) return;
  const source = preview.source;
  elements.previewTitle.textContent = source.pullRequest
    ? `Pull request #${source.pullRequest} benchmark preview`
    : `Manual run ${source.runId} benchmark preview`;
  elements.previewDescription.textContent =
    `${preview.previewRecordCount} result${preview.previewRecordCount === 1 ? "" : "s"} from ` +
    `${source.branch} are overlaid on ${preview.historyRecordCount} durable nightly ` +
    `result${preview.historyRecordCount === 1 ? "" : "s"}. ` +
    `They are not part of benchmark history and expire ${fullDate(preview.expiresAt)}.`;
  const runUrl = safeLink(source.runUrl);
  if (runUrl) {
    elements.previewRunLink.href = runUrl;
  } else {
    elements.previewRunLink.hidden = true;
  }
  elements.preview.hidden = false;
}

function configureSuites() {
  // Union loaded records with the manifest's per-chunk suites so a suite whose
  // runs all predate the eager window is still offered; selecting it loads
  // the pending chunks (see ensureRangeLoaded).
  const suites = [
    ...new Set([
      ...history.records.map((result) => result.identity.suite),
      ...history.manifest.chunks.flatMap((chunk) => chunk.suites ?? []),
    ]),
  ].sort();
  setOptions(
    elements.suite,
    suites.map((suite) => ({ value: suite, label: displayIdentifier(suite) })),
  );
  elements.suiteField.classList.toggle("controls__field--hidden", suites.length <= 1);
}

interface FilterSnapshot {
  gpu: string;
  outcome: string;
  channel: string;
  cases: ReadonlyMap<string, boolean>;
  metrics: ReadonlyMap<string, boolean>;
}

function snapshotFilters(): FilterSnapshot {
  return {
    gpu: elements.gpu.value,
    outcome: elements.outcome.value,
    channel: elements.channel.value,
    cases: checkboxStates(elements.cases),
    metrics: checkboxStates(elements.metrics),
  };
}

// Rebuilding the suite-scoped controls resets them to defaults. Pass
// `preserve` when the rebuild is a side effect of loading more history rather
// than a deliberate suite change or reset, so the user's selections survive;
// values that no longer exist fall back to defaults, new ones get defaults.
function configureSuiteFilters({ preserve = false }: { preserve?: boolean } = {}) {
  const previous = preserve ? snapshotFilters() : null;
  const dimensions = discoverDimensions(history.records, elements.suite.value);
  setOptions(elements.gpu, [
    { value: "all", label: "All GPU models" },
    ...dimensions.gpuModels.map((gpu) => ({ value: gpu, label: gpu })),
  ]);
  setOptions(elements.outcome, [
    { value: "all", label: "All outcomes" },
    ...VALID_OUTCOMES.map((outcome) => ({ value: outcome, label: displayIdentifier(outcome) })),
  ]);
  setOptions(elements.channel, [
    { value: "all", label: "All channels" },
    ...dimensions.channels.map((channel) => ({
      value: channel,
      label: displayIdentifier(channel),
    })),
  ]);
  if (previous) {
    restoreSelection(elements.gpu, previous.gpu);
    restoreSelection(elements.outcome, previous.outcome);
    restoreSelection(elements.channel, previous.channel);
  }
  renderCheckboxes(
    elements.cases,
    dimensions.cases.map((caseName) => ({
      value: caseName,
      label: frameworkLabel(caseName),
      checked: previous?.cases.get(caseName) ?? true,
    })),
    "case",
  );
  renderMetricGroups(dimensions.metrics, previous?.metrics);
}

const METRIC_GROUPS = [
  { label: "Checkpoint", prefix: "checkpoint." },
  { label: "Restore", prefix: "restore." },
] as const;

// Sub-phase measurements whose values are negligible next to the phase
// they're part of (well under a second, against tens of seconds for
// checkpoint/restore) -- selecting them barely moves a stacked bar, they
// just clutter the picker and the chart legend with near-invisible segments.
const NEGLIGIBLE_METRICS: ReadonlySet<string> = new Set([
  "checkpoint.cuda_checkpoint.duration",
  "checkpoint.gpu_device_map.duration",
  "checkpoint.overlay_capture.duration",
  "checkpoint.remove_old_version_and_switch.duration",
  "checkpoint.unaccounted.duration",
  "restore.gpu_device_map.duration",
  "restore.image_pull.duration",
  "restore.image_pull_including_wait.duration",
  "restore.overlay_capture.duration",
  "restore.pagebroker_commit.duration",
  "restore.pagebroker_mount.duration",
  "restore.pagebroker_stage.duration",
  "restore.unaccounted.duration",
]);

// Only the checkpoint/restore measurements are actionable in the stage
// breakdown, so a suite shaped like the framework benchmark drops everything
// else (test.total.duration, source.image_pull*, ...) from the picker rather
// than showing it ungrouped. A suite with none of those measurements at all
// (an unrelated future suite, for example) isn't shaped like that benchmark,
// so it falls back to the old flat, ungrouped list instead of emptying the
// picker -- this is what keeps a newly discovered suite's own measurements
// selectable without a UI code change.
function renderMetricGroups(
  metrics: MetricDefinition[],
  previous?: ReadonlyMap<string, boolean>,
): void {
  elements.metrics.replaceChildren();

  const groups = METRIC_GROUPS
    .map(({ label, prefix }) => ({
      label: label as string | null,
      items: metrics.filter(
        (item) => item.name.startsWith(prefix) && !NEGLIGIBLE_METRICS.has(item.name),
      ),
    }))
    .filter((group) => group.items.length > 0);
  const effectiveGroups = groups.length > 0 ? groups : [{ label: null, items: metrics }];
  // Decide from what is actually shown: a default that only exists among the
  // hidden measurements must not leave the picker with nothing checked.
  const hasDefaults = effectiveGroups.some((group) =>
    group.items.some((item) => DEFAULT_METRIC_NAMES.has(item.name)),
  );

  for (const { label, items } of effectiveGroups) {
    const group = document.createElement("div");
    group.className = "toggle-group";
    if (label) {
      const heading = document.createElement("h4");
      heading.textContent = label;
      group.append(heading);
    }
    const list = document.createElement("div");
    list.className = "toggle-list";
    group.append(list);
    elements.metrics.append(group);

    renderCheckboxes(
      list,
      items.map((item, index) => ({
        value: item.name,
        label: item.displayName,
        checked:
          previous?.get(item.name) ??
          (hasDefaults ? DEFAULT_METRIC_NAMES.has(item.name) : index < 3),
      })),
      "metric",
    );
  }
}

function restoreSelection(select: HTMLSelectElement, value: string): void {
  if ([...select.options].some((option) => option.value === value)) select.value = value;
}

function checkboxStates(container: HTMLElement): Map<string, boolean> {
  return new Map(
    [...container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')].map((input) => [
      input.value,
      input.checked,
    ]),
  );
}

function bindEvents() {
  elements.suite.addEventListener("change", () => {
    void ensureRangeLoaded().then(() => {
      configureSuiteFilters();
      render();
    });
  });
  elements.date.addEventListener("change", () => {
    void ensureRangeLoaded().then(render);
  });
  for (const element of [elements.gpu, elements.outcome, elements.channel]) {
    element.addEventListener("change", render);
  }
  elements.cases.addEventListener("change", render);
  elements.metrics.addEventListener("change", render);
  elements.reset.addEventListener("click", () => {
    elements.date.value = DEFAULT_RANGE;
    elements.suite.selectedIndex = 0;
    void ensureRangeLoaded().then(() => {
      configureSuiteFilters();
      render();
    });
  });
  elements.closeDialog.addEventListener("click", () => elements.dialog.close());
  elements.dialog.addEventListener("click", (event) => {
    if (event.target === elements.dialog) elements.dialog.close();
  });
}

// Never rejects: a failed on-demand load is reported in the status line and
// the caller still re-renders, so the filters and the view stay consistent.
async function ensureRangeLoaded(): Promise<void> {
  if (history.pendingChunks.length === 0) return;
  const selected = elements.date.value;
  const widerThanEager = selected === "all" || Number(selected) > Number(DEFAULT_RANGE);
  if (!widerThanEager && !suiteNeedsPendingChunks(history, elements.suite.value, selectedDays())) {
    // A previous on-demand load may have failed; this view doesn't need it.
    if (elements.status.classList.contains("status--error")) renderStatus();
    return;
  }
  elements.status.textContent = "Loading older benchmark history…";
  try {
    history = await loadRemainingHistory(history);
    configureSuiteFilters({ preserve: true });
    renderStatus();
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    renderStatus();
    elements.status.textContent = `Older benchmark history unavailable: ${message}`;
    elements.status.classList.add("status--error");
  }
}

function selectedDays(): number | null {
  return elements.date.value === "all" ? null : Number(elements.date.value);
}

function render() {
  const selectedCases = checkedValues(elements.cases);
  const selectedMetrics = checkedValues(elements.metrics);
  currentMetrics = selectedMetrics;
  if (history.records.length === 0) {
    elements.count.textContent = "0 results";
    elements.empty.hidden = false;
    elements.empty.textContent =
      "No benchmark results have been published yet. The first scheduled framework run populates this dashboard.";
    renderCharts([], selectedMetrics);
    renderTable([], selectedMetrics);
    return;
  }
  const suiteRecords = history.records.filter(
    (result) => result.identity.suite === elements.suite.value,
  );
  const referenceTime = Math.max(
    ...suiteRecords.map((result) => Date.parse(result.startedAt)),
    0,
  );
  const records = filterRecords(history.records, {
    suite: elements.suite.value,
    cases: selectedCases,
    days: selectedDays(),
    gpu: elements.gpu.value,
    outcome: selectedOutcome(elements.outcome.value),
    channel: elements.channel.value,
    referenceTime,
  });
  elements.count.textContent = `${records.length} result${records.length === 1 ? "" : "s"}`;
  elements.empty.hidden = records.length !== 0;
  elements.empty.textContent = "No benchmark results match the selected filters.";
  renderCharts(records, selectedMetrics);
  renderTable(records, selectedMetrics);
}

function renderCharts(records: BenchmarkResult[], selectedMetrics: ReadonlySet<string>): void {
  for (const chart of charts) chart.destroy();
  charts = [];
  for (const chart of stageComparisonCharts) chart.destroy();
  stageComparisonCharts = [];
  elements.charts.replaceChildren();
  if (records.length === 0) return;
  renderStageComparisonCharts(records, selectedMetrics);
  if (selectedMetrics.size === 0) {
    elements.charts.append(messageCard("Select at least one measurement to draw a chart."));
    return;
  }

  const dimensions = discoverDimensions(history.records, elements.suite.value);
  const metrics = dimensions.metrics.filter((item) => selectedMetrics.has(item.name));
  for (const metric of metrics) {
    const card = document.createElement("article");
    card.className = "chart-card";
    const heading = document.createElement("div");
    heading.className = "chart-card__heading";
    const title = document.createElement("h3");
    title.textContent = metric.displayName;
    const unit = document.createElement("span");
    unit.textContent = humanizeUnit(metric.unit);
    heading.append(title, unit);

    const canvasWrap = document.createElement("div");
    canvasWrap.className = "chart-canvas";
    const canvas = document.createElement("canvas");
    canvas.setAttribute(
      "aria-label",
      `${metric.displayName} time series by benchmark case, in ${humanizeUnit(metric.unit)}`,
    );
    canvas.setAttribute("role", "img");
    canvasWrap.append(canvas);
    card.append(heading, canvasWrap);

    const gaps = records.filter((result) => measurement(result, metric.name)?.status !== "complete");
    if (gaps.length) {
      const strip = document.createElement("div");
      strip.className = "failure-strip";
      const label = document.createElement("strong");
      label.textContent = "Explicit gaps:";
      strip.append(label);
      for (const result of gaps) {
        const item = measurement(result, metric.name);
        const reason =
          item?.status === "incomplete" ? item.missingReason : "measurement not recorded";
        const button = document.createElement("button");
        button.type = "button";
        button.textContent = `${frameworkLabel(result.identity.case)} · ${displayIdentifier(result.outcome)} · ${shortDate(result.startedAt)} · ${reason}`;
        button.addEventListener("click", () => showDetails(result));
        strip.append(button);
      }
      card.append(strip);
    }
    elements.charts.append(card);

    const datasets = seriesForMetric(records, metric.name, history.records).map((series) =>
      chartDataset(series, metric),
    );
    charts.push(
      new Chart(canvas, {
        type: "line",
        data: { datasets },
        options: chartOptions(metric),
      }),
    );
  }
}

function chartDataset(
  series: MetricSeries,
  metric: MetricDefinition,
): ChartDataset<"line", DashboardPoint[]> {
  const color = frameworkColor(series.case);
  return {
    label: frameworkLabel(series.case),
    data: series.points.map((point): DashboardPoint => ({ ...point, metric })),
    parsing: false,
    borderColor: color,
    backgroundColor: color,
    borderWidth: 2,
    tension: 0.18,
    spanGaps: false,
    pointRadius: 4,
    pointHoverRadius: 7,
    pointBorderWidth: 2,
    pointBackgroundColor: (context) =>
      chartPoint(context)?.outcome === "passed" ? color : "#e24a3b",
    pointBorderColor: (context) =>
      chartPoint(context)?.outcome === "passed" ? "#ffffff" : "#7d2018",
    pointStyle: (context) =>
      chartPoint(context)?.outcome === "passed" ? "circle" : "crossRot",
  };
}

function chartOptions(metric: MetricDefinition): ChartOptions<"line"> {
  return {
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    interaction: { intersect: false, mode: "nearest" },
    onClick: (_event, activeElements, chart) => {
      const active = activeElements[0];
      if (!active) return;
      const point = chart.data.datasets[active.datasetIndex]?.data[active.index];
      if (point && typeof point === "object" && "result" in point) {
        showDetails((point as DashboardPoint).result);
      }
    },
    plugins: {
      legend: {
        position: "bottom",
        labels: { usePointStyle: true, boxWidth: 10, padding: 18 },
      },
      tooltip: {
        callbacks: {
          title: (items) => {
            const point = tooltipPoint(items[0]);
            return point ? fullDate(point.result.startedAt) : "";
          },
          label: (context) => {
            const point = tooltipPoint(context);
            return point ? tooltipLines(point, metric) : [];
          },
        },
      },
    },
    scales: {
      x: {
        type: "linear",
        ticks: {
          callback: (value) => shortDate(new Date(Number(value)).toISOString()),
        },
        grid: { color: "rgba(42, 53, 56, 0.08)" },
      },
      y: {
        beginAtZero: true,
        title: { display: true, text: humanizeUnit(metric.unit) },
        grid: { color: "rgba(42, 53, 56, 0.08)" },
      },
    },
  };
}

function tooltipLines(point: DashboardPoint, metric: MetricDefinition): string[] {
  const environment = point.result.environment;
  return [
    `${frameworkLabel(point.result.identity.case)}: ${formatValue(point.y, metric.unit)}`,
    `Outcome: ${displayIdentifier(point.outcome)}`,
    ...(point.comparison.skippedReason
      ? [`Comparison: ${point.comparison.skippedReason}`]
      : [
          `Previous: ${formatComparison(point.comparison.previous, metric.unit)}`,
          `Median (last 7): ${formatComparison(point.comparison.median7, metric.unit)}`,
        ]),
    `GPU: ${gpuModels(point.result).join(", ") || "unknown"}`,
    `Commit: ${shortCommit(point.result) ?? "unknown"}`,
    `Snapshot: ${stringProperty(point.result.source, "snapshotTag") ?? "unknown"}`,
    `Framework image: ${stringProperty(environment, "frameworkImage") ?? "unknown"}`,
    "Click the point for run details and links",
  ];
}

function renderTable(
  records: BenchmarkResult[],
  selectedMetrics: ReadonlySet<string>,
): void {
  const dimensions = discoverDimensions(history.records, elements.suite.value);
  const metrics = dimensions.metrics.filter((item) => selectedMetrics.has(item.name));
  const header = document.createElement("tr");
  for (const title of ["Started", "Case", "Outcome", "GPU", ...metrics.map((item) => item.displayName), "Run"]) {
    const cell = document.createElement("th");
    cell.scope = "col";
    cell.textContent = title;
    header.append(cell);
  }
  elements.tableHead.replaceChildren(header);
  elements.tableBody.replaceChildren();

  const latest = [...records]
    .sort((left, right) => Date.parse(right.startedAt) - Date.parse(left.startedAt))
    .slice(0, 25);
  for (const result of latest) {
    const row = document.createElement("tr");
    row.className = `outcome--${result.outcome}`;
    appendCell(row, fullDate(result.startedAt));
    appendCell(row, frameworkLabel(result.identity.case));
    appendOutcomeCell(row, result.outcome);
    appendCell(row, gpuModels(result).join(", ") || "unknown");
    for (const metric of metrics) {
      const item = measurement(result, metric.name);
      appendCell(
        row,
        item?.status === "complete"
          ? formatValue(item.value, item.unit)
          : item?.missingReason || "—",
      );
    }
    const action = document.createElement("td");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "table-action";
    button.textContent = "Details";
    button.addEventListener("click", () => showDetails(result));
    action.append(button);
    row.append(action);
    elements.tableBody.append(row);
  }
}

function showDetails(result: BenchmarkResult): void {
  elements.dialogContent.replaceChildren();
  const summary = document.createElement("dl");
  summary.className = "details-grid";
  const environment = result.environment;
  const storage = environment.storage;
  const storageType = stringProperty(storage, "type") ?? "unknown";
  const storageSize = stringProperty(storage, "requestedSize") ?? "unknown size";
  const fields: Array<readonly [string, string]> = [
    ["Suite", result.identity.suite],
    ["Case", frameworkLabel(result.identity.case)],
    ["Outcome", displayIdentifier(result.outcome)],
    ["Started", fullDate(result.startedAt)],
    ["Run", `${result.identity.runId} (attempt ${result.identity.runAttempt})`],
    ["GPU", gpuModels(result).join(", ") || "unknown"],
    ["Model", stringProperty(environment, "model") ?? "unknown"],
    ["Storage", `${storageType} · ${storageSize}`],
    ["Snapshot tag", stringProperty(result.source, "snapshotTag") ?? "unknown"],
    ["Framework image", stringProperty(environment, "frameworkImage") ?? "unknown"],
    ["Image digest", stringProperty(environment, "frameworkImageDigest") ?? "unknown"],
    ["Commit", stringProperty(result.source, "commit") ?? "unknown"],
  ];
  for (const [label, value] of fields) appendDefinition(summary, label, value);
  elements.dialogContent.append(summary);

  renderStageBreakdown(result);

  const heading = document.createElement("h3");
  heading.textContent = "Measurements";
  const list = document.createElement("dl");
  list.className = "measurement-list";
  for (const { measurement: item, comparison } of measurementComparisons(result, history.records)) {
    appendDefinition(
      list,
      item.displayName,
      item.status === "complete"
        ? comparison.skippedReason
          ? `${formatValue(item.value, item.unit)} · ${comparison.skippedReason}`
          : `${formatValue(item.value, item.unit)} · previous ${formatComparison(comparison.previous, item.unit)} · median (last 7) ${formatComparison(comparison.median7, item.unit)}`
        : `Incomplete · ${item.missingReason}`,
    );
  }
  elements.dialogContent.append(heading, list);

  const links = document.createElement("p");
  links.className = "details-links";
  const runUrl = safeLink(result.source.runUrl);
  if (runUrl) links.append(linkButton(runUrl, "Open GitHub Actions run"));
  const commit = commitUrl(result);
  if (commit) links.append(linkButton(commit, "Open commit"));
  if (links.childElementCount) elements.dialogContent.append(links);
  elements.dialog.showModal();
}

// The stage chart only makes sense for the checkpoint/restore benchmark
// shape; a suite whose selected measurements are all something else (a
// single-value throughput metric, say) has no business getting a "stage
// breakdown" bar with one meaningless segment.
function hasStageShapedMetrics(names: Iterable<string>): boolean {
  for (const name of names) {
    if (METRIC_GROUPS.some((group) => name.startsWith(group.prefix))) return true;
  }
  return false;
}

function renderStageComparisonCharts(
  records: BenchmarkResult[],
  selectedMetrics: ReadonlySet<string>,
): void {
  if (!hasStageShapedMetrics(selectedMetrics)) return;
  const cases = [...new Set(records.map((result) => result.identity.case))].sort();
  for (const caseName of cases) {
    const comparison = recentStageComparison(
      records,
      caseName,
      STAGE_COMPARISON_RUN_COUNT,
      selectedMetrics,
    );
    if (comparison.runs.length === 0 || comparison.stages.length === 0) continue;

    const card = document.createElement("article");
    card.className = "chart-card";
    const heading = document.createElement("div");
    heading.className = "chart-card__heading";
    const title = document.createElement("h3");
    title.textContent = `${frameworkLabel(caseName)} stage breakdown`;
    const unit = document.createElement("span");
    unit.textContent = `last ${comparison.runs.length} run${comparison.runs.length === 1 ? "" : "s"}`;
    heading.append(title, unit);

    const canvasWrap = document.createElement("div");
    canvasWrap.className = "chart-canvas chart-canvas--stage-comparison";
    const canvas = document.createElement("canvas");
    canvas.setAttribute(
      "aria-label",
      `${frameworkLabel(caseName)} stage breakdown for the last ${comparison.runs.length} runs, in seconds`,
    );
    canvas.setAttribute("role", "img");
    canvasWrap.append(canvas);
    card.append(heading, canvasWrap);
    elements.charts.append(card);

    // Index 0 is the most recent run; reverse so the chart reads oldest to
    // newest top-to-bottom, matching the line charts' left-to-right time axis.
    const runs = [...comparison.runs].reverse();
    const labels = runs.map((run) => stageRunLabel(run.result));
    const chart = new Chart(canvas, {
      type: "bar",
      data: {
        labels,
        datasets: comparison.stages.map((stage): ChartDataset<"bar", number[]> => ({
          label: stage.displayName,
          data: runs.map((run) => run.values.get(stage.name) ?? 0),
          backgroundColor: stageColor(stage.name),
          stack: "timeline",
        })),
      },
      options: {
        ...stageChartOptions(),
        onClick: (_event, activeElements) => {
          const active = activeElements[0];
          if (!active) return;
          const run = runs[active.index];
          if (run) showDetails(run.result);
        },
      },
    });
    stageComparisonCharts.push(chart);
  }
}

function stageRunLabel(result: BenchmarkResult): string {
  const outcome = result.outcome === "passed" ? "" : ` · ${displayIdentifier(result.outcome)}`;
  return `${shortDate(result.startedAt)}${outcome}`;
}

function renderStageBreakdown(result: BenchmarkResult): void {
  stageChart?.destroy();
  stageChart = null;
  if (!hasStageShapedMetrics(currentMetrics)) return;
  const segments = measurementStageSegments(result, currentMetrics);
  if (segments.length === 0) return;

  const heading = document.createElement("h3");
  heading.textContent = "Stage breakdown";
  const total = segments.reduce((sum, segment) => sum + segment.seconds, 0);
  const caption = document.createElement("p");
  caption.className = "stage-breakdown__caption";
  caption.textContent = `${formatValue(total, "seconds")} total, from ${segments.length} stage${segments.length === 1 ? "" : "s"}.`;

  const canvasWrap = document.createElement("div");
  canvasWrap.className = "chart-canvas chart-canvas--stage";
  const canvas = document.createElement("canvas");
  canvas.setAttribute("aria-label", "Stage breakdown of total run duration, in seconds");
  canvas.setAttribute("role", "img");
  canvasWrap.append(canvas);

  elements.dialogContent.append(heading, caption, canvasWrap);

  stageChart = new Chart(canvas, {
    type: "bar",
    data: {
      labels: ["Timeline"],
      datasets: segments.map((segment): ChartDataset<"bar", number[]> => ({
        label: segment.displayName,
        data: [segment.seconds],
        backgroundColor: stageColor(segment.name),
        stack: "timeline",
      })),
    },
    options: stageChartOptions(),
  });
}

function stageChartOptions(): ChartOptions<"bar"> {
  return {
    indexAxis: "y",
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    plugins: {
      legend: {
        position: "bottom",
        labels: { usePointStyle: true, boxWidth: 10, padding: 12 },
      },
      tooltip: {
        callbacks: {
          label: (context) => {
            const segment = (context.dataset as { label?: string }).label ?? "";
            const value = context.parsed.x ?? 0;
            return `${segment}: ${formatValue(value, "seconds")}`;
          },
        },
      },
    },
    scales: {
      x: {
        stacked: true,
        beginAtZero: true,
        title: { display: true, text: "seconds" },
        grid: { color: "rgba(42, 53, 56, 0.08)" },
      },
      y: {
        stacked: true,
        grid: { display: false },
      },
    },
  };
}

function linkButton(href: string, label: string): HTMLAnchorElement {
  const link = document.createElement("a");
  link.className = "button";
  link.href = href;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  link.textContent = label;
  return link;
}

function renderCheckboxes(
  container: HTMLElement,
  items: CheckboxOption[],
  name: string,
): void {
  container.replaceChildren();
  for (const item of items) {
    const label = document.createElement("label");
    label.className = "toggle";
    const input = document.createElement("input");
    input.type = "checkbox";
    input.name = name;
    input.value = item.value;
    input.checked = item.checked;
    const text = document.createElement("span");
    text.textContent = item.label;
    label.append(input, text);
    container.append(label);
  }
}

function setOptions(select: HTMLSelectElement, options: SelectOption[]): void {
  select.replaceChildren();
  for (const option of options) {
    const element = document.createElement("option");
    element.value = option.value;
    element.textContent = option.label;
    select.append(element);
  }
}

function checkedValues(container: HTMLElement): Set<string> {
  return new Set(
    [...container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]:checked')].map(
      (input) => input.value,
    ),
  );
}

function appendCell(row: HTMLTableRowElement, value: string): void {
  const cell = document.createElement("td");
  cell.textContent = value;
  row.append(cell);
}

function appendOutcomeCell(row: HTMLTableRowElement, outcome: Outcome): void {
  const cell = document.createElement("td");
  const badge = document.createElement("span");
  badge.className = `badge badge--${outcome}`;
  badge.textContent = displayIdentifier(outcome);
  cell.append(badge);
  row.append(cell);
}

function appendDefinition(
  list: HTMLDListElement,
  label: string,
  value: string,
): void {
  const term = document.createElement("dt");
  term.textContent = label;
  const detail = document.createElement("dd");
  detail.textContent = value;
  list.append(term, detail);
}

function messageCard(message: string): HTMLParagraphElement {
  const element = document.createElement("p");
  element.className = "empty-state";
  element.textContent = message;
  return element;
}

function formatComparison(
  comparison: MetricComparison["previous"] | MetricComparison["median7"],
  unit: string,
): string {
  if (!comparison) return "no comparable baseline";
  return `${formatValue(comparison.value, unit)} (${formatDelta(comparison.deltaPercent)})`;
}

function frameworkColor(caseName: string): string {
  return COLORS[caseName] ?? FALLBACK_COLORS[caseColorIndex(caseName, FALLBACK_COLORS.length)]!;
}

function frameworkLabel(value: string): string {
  if (value === "vllm") return "vLLM";
  if (value === "sglang") return "SGLang";
  if (value === "tensorrt-llm") return "TensorRT-LLM";
  return displayIdentifier(value);
}

function displayIdentifier(value: string): string {
  return value
    .replaceAll("_", " ")
    .replaceAll("-", " ")
    .replace(/\b\w/g, (character) => character.toUpperCase());
}

function shortDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(
    new Date(value),
  );
}

function fullDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

function selectedOutcome(value: string): Outcome | "all" {
  return VALID_OUTCOMES.find((outcome) => outcome === value) ?? "all";
}

function chartPoint(context: ScriptableContext<"line">): DashboardPoint | null {
  return context.raw && typeof context.raw === "object"
    ? (context.raw as DashboardPoint)
    : null;
}

function tooltipPoint(item: TooltipItem<"line"> | undefined): DashboardPoint | null {
  return item?.raw && typeof item.raw === "object"
    ? (item.raw as DashboardPoint)
    : null;
}

function requiredElement<T extends Element>(selector: string): T {
  const element = document.querySelector<T>(selector);
  if (!element) {
    throw new Error(`Required dashboard element ${selector} was not found`);
  }
  return element;
}

start();
