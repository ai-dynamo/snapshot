<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# io_uring serving benchmark report and analysis

[Read the independent benchmark report](REPORT.md).

This folder packages the report and derived analysis for the three model sweeps and the targeted Dynamo restore follow-up. It contains no raw request exports, live cluster credentials or runnable cluster-control environment. Original experiment archives remain in their existing locations.

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

The packaged `tail_report.py` records the follow-up analysis method. Recomputing it from raw requests requires the original experiment archives and their expected directory layout; it is not a standalone command for this report package. Its completed results are included as `analysis/tail-results.json`.

Paths and run identifiers in original provenance files refer to the original experiment archives. No benchmark, cleanup, credential or cluster-control action is needed to inspect this package.

## Follow-up provenance

The [frozen continuation plan](analysis/tail/formal-plan.json) covers repeats
6–10: its `measurement_cells: 40` counts only the continuation. The earlier
five pairs contribute 40 cells, for 80 cells in the full ten-pair protocol, as
recorded in [the continuation source](analysis/tail/continuation-source.json).
The frozen plan is preserved byte for byte so its hash still matches
`plan_sha256` in the freeze record.

The source record hashes the earlier segment's progress, not the continuation's
own progress. Both `formal-progress.json` files and the raw request exports
remain outside this package. No continuation progress hash is available here;
the package manifest verifies the packaged files only. Recomputing the follow-up
and verifying that continuation require the original archives.
