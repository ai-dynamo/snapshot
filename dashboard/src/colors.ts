// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { caseColorIndex } from "./data.ts";

export const FRAMEWORK_COLORS: Readonly<Record<string, string>> = {
  vllm: "#0f766e",
  sglang: "#2563eb",
  "tensorrt-llm": "#d97706",
};

export const FRAMEWORK_FALLBACK_COLORS = ["#7c3aed", "#db2777", "#0891b2", "#65a30d"] as const;

export const CHECKPOINT_STAGE_COLORS: Readonly<Record<string, string>> = {
  "checkpoint.duration": "#16736f",
  "checkpoint.agent.duration": "#4f6db8",
  "checkpoint.criu_dump.duration": "#2f7d43",
  "checkpoint.gpu_device_map.duration": "#53616f",
  "checkpoint.cuda_checkpoint.duration": "#46a5a0",
  "checkpoint.overlay_capture.duration": "#7d9eed",
  "checkpoint.remove_old_version_and_switch.duration": "#5ab06d",
  "checkpoint.unaccounted.duration": "#7b91a6",
};

export const RESTORE_STAGE_COLORS: Readonly<Record<string, string>> = {
  "restore.to_traffic.duration": "#c85a32",
  "restore.pod_create_to_traffic.duration": "#a34d73",
  "restore.agent.duration": "#6f5499",
  "restore.criu_restore.duration": "#9b6a1c",
  "restore.pagebroker_stage.duration": "#f58e6a",
  "restore.pagebroker_mount.duration": "#d07e9f",
  "restore.pagebroker_commit.duration": "#9a82c4",
  "restore.gpu_device_map.duration": "#c7995a",
  "restore.overlay_capture.duration": "#9c3100",
  "restore.criu_prepare.duration": "#79274e",
  "restore.cuda_restore.duration": "#4b3071",
  "restore.unaccounted.duration": "#734500",
};

export const NEUTRAL_STAGE_COLORS: Readonly<Record<string, string>> = {
  "test.total.duration": "#262522",
  "restore.agent_complete_to_traffic.duration": "#3f3d3a",
  "restore.image_pull.duration": "#5a5854",
  "source.image_pull.duration": "#767370",
  "restore.image_pull_including_wait.duration": "#93908b",
  "source.image_pull_including_wait.duration": "#b1ada7",
};

export const STAGE_COLORS: Readonly<Record<string, string>> = {
  ...CHECKPOINT_STAGE_COLORS,
  ...RESTORE_STAGE_COLORS,
  ...NEUTRAL_STAGE_COLORS,
};

export const UNKNOWN_STAGE_COLORS = ["#8c7b6b", "#6b7a8c", "#8c6b83"] as const;

export function frameworkColor(caseName: string): string {
  return (
    FRAMEWORK_COLORS[caseName] ??
    FRAMEWORK_FALLBACK_COLORS[caseColorIndex(caseName, FRAMEWORK_FALLBACK_COLORS.length)]!
  );
}

export function stageColor(name: string): string {
  return STAGE_COLORS[name] ?? UNKNOWN_STAGE_COLORS[caseColorIndex(name, UNKNOWN_STAGE_COLORS.length)]!;
}
