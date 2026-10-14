// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import {
  CHECKPOINT_STAGE_COLORS,
  FRAMEWORK_COLORS,
  FRAMEWORK_FALLBACK_COLORS,
  NEUTRAL_STAGE_COLORS,
  RESTORE_STAGE_COLORS,
  STAGE_COLORS,
  UNKNOWN_STAGE_COLORS,
  frameworkColor,
  stageColor,
} from "./colors.ts";

const HEX = /^#[0-9a-f]{6}$/;

function oklab(hex: string): [number, number, number] {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  return [
    0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  ];
}

function deltaE(a: string, b: string): number {
  const x = oklab(a);
  const y = oklab(b);
  return 100 * Math.hypot(x[0] - y[0], x[1] - y[1], x[2] - y[2]);
}

function minPairwiseDeltaE(colors: readonly string[]): number {
  let min = Number.POSITIVE_INFINITY;
  for (let i = 0; i < colors.length; i++) {
    for (let j = i + 1; j < colors.length; j++) {
      min = Math.min(min, deltaE(colors[i]!, colors[j]!));
    }
  }
  return min;
}

describe("framework colors", () => {
  it("gives every known framework and fallback a distinct valid color", () => {
    const all = [...Object.values(FRAMEWORK_COLORS), ...FRAMEWORK_FALLBACK_COLORS];
    expect(new Set(all).size).toBe(all.length);
    for (const color of all) expect(color).toMatch(HEX);
  });

  it("keeps framework colors apart from each other", () => {
    expect(minPairwiseDeltaE(Object.values(FRAMEWORK_COLORS))).toBeGreaterThanOrEqual(15);
  });

  it("is deterministic for unknown cases and never reuses a known framework color", () => {
    expect(frameworkColor("triton")).toBe(frameworkColor("triton"));
    expect(Object.values(FRAMEWORK_COLORS)).not.toContain(frameworkColor("triton"));
    expect(frameworkColor("vllm")).toBe(FRAMEWORK_COLORS["vllm"]);
  });
});

describe("stage colors", () => {
  it("assigns every known measurement its own color", () => {
    const all = Object.values(STAGE_COLORS);
    expect(new Set(all).size).toBe(all.length);
    for (const color of all) expect(color).toMatch(HEX);
    expect(Object.keys(STAGE_COLORS)).toHaveLength(
      Object.keys(CHECKPOINT_STAGE_COLORS).length +
        Object.keys(RESTORE_STAGE_COLORS).length +
        Object.keys(NEUTRAL_STAGE_COLORS).length,
    );
  });

  it("keeps the primary checkpoint and restore stages easy to tell apart", () => {
    const primary = [
      STAGE_COLORS["checkpoint.duration"]!,
      STAGE_COLORS["checkpoint.agent.duration"]!,
      STAGE_COLORS["checkpoint.criu_dump.duration"]!,
      STAGE_COLORS["restore.to_traffic.duration"]!,
      STAGE_COLORS["restore.pod_create_to_traffic.duration"]!,
      STAGE_COLORS["restore.agent.duration"]!,
      STAGE_COLORS["restore.criu_restore.duration"]!,
    ];
    expect(minPairwiseDeltaE(primary)).toBeGreaterThanOrEqual(8);
  });

  it("keeps every color within a phase family distinguishable", () => {
    expect(minPairwiseDeltaE(Object.values(CHECKPOINT_STAGE_COLORS))).toBeGreaterThanOrEqual(7);
    expect(minPairwiseDeltaE(Object.values(RESTORE_STAGE_COLORS))).toBeGreaterThanOrEqual(7);
    expect(minPairwiseDeltaE(Object.values(NEUTRAL_STAGE_COLORS))).toBeGreaterThanOrEqual(7);
  });

  it("groups measurements by their phase prefix", () => {
    for (const name of Object.keys(CHECKPOINT_STAGE_COLORS)) expect(name).toMatch(/^checkpoint\./);
    for (const name of Object.keys(RESTORE_STAGE_COLORS)) expect(name).toMatch(/^restore\./);
  });

  it("never gives an unknown measurement a known stage color, and is deterministic", () => {
    const unknown = "restore.future_phase.duration";
    expect(stageColor(unknown)).toBe(stageColor(unknown));
    expect(UNKNOWN_STAGE_COLORS).toContain(stageColor(unknown));
    expect(Object.values(STAGE_COLORS)).not.toContain(stageColor(unknown));
    expect(stageColor("checkpoint.duration")).toBe(CHECKPOINT_STAGE_COLORS["checkpoint.duration"]);
  });
});
