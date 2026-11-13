<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# io_uring serving benchmark report and analysis

[Read the independent benchmark report](REPORT.md).

This folder packages the report and derived analysis for the three model sweeps and the targeted Dynamo restore follow-up. It contains no raw request exports, live cluster credentials or runnable cluster-control environment.

## Contents

| File or directory | Purpose |
|---|---|
| [REPORT.md](REPORT.md) | Complete methods, results, non-throughput analysis and limitations |
| [analysis/all-model-paired-metrics.csv](analysis/all-model-paired-metrics.csv) | 3,736 paired metric comparisons across models, systems and workloads |
| [analysis/all-model-paired-metrics.json](analysis/all-model-paired-metrics.json) | Same comparisons with methods and source hashes |
| [analysis/validation.json](analysis/validation.json) | Consolidation validation, including 208 cross-checked primary estimates |
| `analysis/0.6b/`, `analysis/8b/`, `analysis/27b/` | Original per-model summaries and frozen model/runtime metadata |
| [analysis/metrics-comparison.json](analysis/metrics-comparison.json) | Recalculated 27B per-run latency and aligned CPU/request analysis |
| [analysis/tail-results.json](analysis/tail-results.json) | Combined ten-pair restore follow-up, including windows and sensitivity checks |
| `analysis/tail/initial-runs.json`, `analysis/tail/continuation-runs.json` | Saved per-run window metrics and execution metadata for the two segments |
| [analysis/final-analysis-validation.json](analysis/final-analysis-validation.json) | Follow-up raw-export hash and recomputation verification receipt |
| `analysis/tail/` | Follow-up experiment metadata and continuation provenance |
| [SHA256SUMS.json](SHA256SUMS.json) | Package file-integrity manifest |

The per-model `RESULTS.md` files retain the experiment summaries and numerical
results, with package notes clarifying archive references and completed phases.
The independent synthesis is `REPORT.md`.

## Recompute the consolidated comparisons

Using Python 3 and the packaged per-run analysis inputs:

```sh
python3 analysis/consolidate.py
```

This recomputes paired estimates and confidence intervals and rewrites the consolidated JSON/CSV. Regenerate the integrity manifest if intentional file changes are made afterward. Comparisons without five positive finite metric pairs are omitted; absent token-spacing metrics are not zero.

Recompute the follow-up paired estimates from the packaged per-run window
metrics:

```sh
python3 analysis/tail_report.py
```

The script verifies both input hashes in
[the segment index](analysis/tail/continuation-source.json) and rewrites
`analysis/tail-results.json`. The input files preserve the previously saved
window metrics; they do not contain raw request exports. This command therefore
recomputes paired ratios, confidence intervals and sensitivity subsets, but does
not recalculate request-level windows or percentiles.

Paths and run identifiers in original provenance files refer to the original experiment archives. No benchmark, cleanup, credential or cluster-control action is needed to inspect this package.

## Follow-up provenance

The [frozen continuation plan](analysis/tail/formal-plan.json) covers repeats
6–10: its `measurement_cells: 40` counts only the continuation. The earlier
five pairs contribute 40 cells, for 80 cells in the full ten-pair protocol, as
recorded in [the continuation source](analysis/tail/continuation-source.json).
The frozen plan is preserved byte for byte so its hash still matches
`plan_sha256` in the freeze record.

The segment index identifies and hashes both packaged per-run input files.
`initial-runs.json` covers repeats 1–5 (`formal-tail-` cell names), and
`continuation-runs.json` covers repeats 6–10 (`formal-tc2-` cell names). Each
file retains its original execution directory and events. These inputs were
extracted from the saved `runs` and `execution_segments` in `tail-results.json`;
their hashes verify the packaged inputs, not the original raw requests.
