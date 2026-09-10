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
  VALID_OUTCOMES,
  discoverDimensions,
  filterRecords,
  formatDelta,
  formatValue,
  gpuModels,
  loadHistory,
  measurement,
  runChannel,
  safeLink,
  seriesForMetric,
  stringProperty,
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
import "./style.css";

Chart.register(...registerables);

const COLORS: Readonly<Record<string, string>> = {
  vllm: "#76b900",
  sglang: "#5d7fe5",
  "tensorrt-llm": "#ef9f27",
};
const FALLBACK_COLORS = ["#18a999", "#b05fd3", "#e05a47", "#517891"] as const;
const DEFAULT_METRIC_NAMES: ReadonlySet<string> = new Set(DEFAULT_METRICS);

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
  status: requiredElement<HTMLElement>("#load-status"),
  suite: requiredElement<HTMLSelectElement>("#suite-filter"),
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

async function start() {
  try {
    history = await loadHistory(new URL("./", document.baseURI));
    configureSuites();
    configureSuiteFilters();
    bindEvents();
    render();
    const warningText = history.warnings.length
      ? ` · ${history.warnings.length} record warning${history.warnings.length === 1 ? "" : "s"}`
      : "";
    elements.status.textContent =
      `Loaded ${history.records.length} benchmark results from ` +
      `${history.manifest.chunks.length} monthly index${history.manifest.chunks.length === 1 ? "" : "es"}${warningText}.`;
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    elements.status.textContent = `Benchmark history unavailable: ${message}`;
    elements.status.classList.add("status--error");
    elements.empty.hidden = false;
    elements.empty.textContent = "The benchmark data could not be loaded.";
  }
}

function configureSuites() {
  const suites = [...new Set(history.records.map((result) => result.identity.suite))].sort();
  setOptions(
    elements.suite,
    suites.map((suite) => ({ value: suite, label: displayIdentifier(suite) })),
  );
}

function configureSuiteFilters() {
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
  renderCheckboxes(
    elements.cases,
    dimensions.cases.map((caseName) => ({
      value: caseName,
      label: frameworkLabel(caseName),
      checked: true,
    })),
    "case",
  );
  const hasDefaults = dimensions.metrics.some((item) => DEFAULT_METRIC_NAMES.has(item.name));
  renderCheckboxes(
    elements.metrics,
    dimensions.metrics.map((item, index) => ({
      value: item.name,
      label: item.displayName,
      checked: hasDefaults ? DEFAULT_METRIC_NAMES.has(item.name) : index < 3,
    })),
    "metric",
  );
}

function bindEvents() {
  elements.suite.addEventListener("change", () => {
    configureSuiteFilters();
    render();
  });
  for (const element of [elements.date, elements.gpu, elements.outcome, elements.channel]) {
    element.addEventListener("change", render);
  }
  elements.cases.addEventListener("change", render);
  elements.metrics.addEventListener("change", render);
  elements.reset.addEventListener("click", () => {
    elements.date.value = "90";
    configureSuiteFilters();
    render();
  });
  elements.closeDialog.addEventListener("click", () => elements.dialog.close());
  elements.dialog.addEventListener("click", (event) => {
    if (event.target === elements.dialog) elements.dialog.close();
  });
}

function render() {
  const selectedCases = checkedValues(elements.cases);
  const selectedMetrics = checkedValues(elements.metrics);
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
    days: elements.date.value === "all" ? null : Number(elements.date.value),
    gpu: elements.gpu.value,
    outcome: selectedOutcome(elements.outcome.value),
    channel: elements.channel.value,
    referenceTime,
  });
  elements.count.textContent = `${records.length} result${records.length === 1 ? "" : "s"}`;
  elements.empty.hidden = records.length !== 0;
  renderCharts(records, selectedMetrics, suiteRecords);
  renderTable(records, selectedMetrics);
}

function renderCharts(
  records: BenchmarkResult[],
  selectedMetrics: ReadonlySet<string>,
  comparisonHistory: BenchmarkResult[],
): void {
  for (const chart of charts) chart.destroy();
  charts = [];
  elements.charts.replaceChildren();
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
    unit.textContent = metric.unit;
    heading.append(title, unit);

    const canvasWrap = document.createElement("div");
    canvasWrap.className = "chart-canvas";
    const canvas = document.createElement("canvas");
    canvas.setAttribute(
      "aria-label",
      `${metric.displayName} time series by benchmark case, in ${metric.unit}`,
    );
    canvas.setAttribute("role", "img");
    canvasWrap.append(canvas);
    card.append(heading, canvasWrap);

    const incomplete = records.filter((result) => {
      const item = measurement(result, metric.name);
      return result.outcome !== "passed" && item?.status !== "complete";
    });
    if (incomplete.length) {
      const strip = document.createElement("div");
      strip.className = "failure-strip";
      const label = document.createElement("strong");
      label.textContent = "Explicit gaps:";
      strip.append(label);
      for (const result of incomplete) {
        const button = document.createElement("button");
        button.type = "button";
        button.textContent = `${frameworkLabel(result.identity.case)} · ${displayIdentifier(result.outcome)} · ${shortDate(result.startedAt)}`;
        button.addEventListener("click", () => showDetails(result));
        strip.append(button);
      }
      card.append(strip);
    }
    elements.charts.append(card);

    const datasets = seriesForMetric(records, metric.name, comparisonHistory).map(
      (series, index) => chartDataset(series, index, metric),
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
  index: number,
  metric: MetricDefinition,
): ChartDataset<"line", DashboardPoint[]> {
  const color = frameworkColor(series.case, index);
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
        title: { display: true, text: metric.unit },
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
    `Previous: ${formatComparison(point.comparison.previous, metric.unit)}`,
    `Median (last 7): ${formatComparison(point.comparison.median7, metric.unit)}`,
    `GPU: ${gpuModels(point.result).join(", ") || "unknown"}`,
    `Snapshot: ${stringProperty(point.result.source, "snapshotTag") ?? "unknown"}`,
    `Framework image: ${stringProperty(environment, "frameworkImage") ?? "unknown"}`,
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
    ["GPU", gpuModels(result).join(", ") || "unknown"],
    ["Model", stringProperty(environment, "model") ?? "unknown"],
    ["Storage", `${storageType} · ${storageSize}`],
    ["Snapshot tag", stringProperty(result.source, "snapshotTag") ?? "unknown"],
    ["Framework image", stringProperty(environment, "frameworkImage") ?? "unknown"],
    ["Commit", stringProperty(result.source, "commit") ?? "unknown"],
  ];
  for (const [label, value] of fields) appendDefinition(summary, label, value);
  elements.dialogContent.append(summary);

  const heading = document.createElement("h3");
  heading.textContent = "Measurements";
  const list = document.createElement("dl");
  list.className = "measurement-list";
  for (const item of result.measurements) {
    appendDefinition(
      list,
      item.displayName,
      item.status === "complete"
        ? formatValue(item.value, item.unit)
        : `Incomplete · ${item.missingReason}`,
    );
  }
  elements.dialogContent.append(heading, list);
  const runUrl = safeLink(result.source.runUrl);
  if (runUrl) {
    const link = document.createElement("a");
    link.className = "button";
    link.href = runUrl;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent = "Open GitHub Actions run";
    elements.dialogContent.append(link);
  }
  elements.dialog.showModal();
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

function frameworkColor(caseName: string, index: number): string {
  return COLORS[caseName] ?? FALLBACK_COLORS[index % FALLBACK_COLORS.length]!;
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
