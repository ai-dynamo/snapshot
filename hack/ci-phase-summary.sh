#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Render the phase log written by ci-phase.sh as a markdown table.
#
# Prints the share of the job each phase took, because the question issue 505
# asks is not how long setup takes but how much of the job it is. The total is
# the sum of the recorded phases, not the job's wall time: steps that are not
# instrumented are deliberately absent rather than silently folded in.
#
# Usage: ci-phase-summary.sh [cache-state]

set -eu

: "${CI_PHASE_LOG:?CI_PHASE_LOG must name the file to read}"

cache_state="${1:-unknown}"

if [ ! -s "$CI_PHASE_LOG" ]; then
    echo "### Job phases"
    echo
    echo "No phases were recorded; the job stopped before the first one finished."
    exit 0
fi

echo "### Job phases"
echo
echo "Cache: ${cache_state}. Percentages are of the instrumented phases below,"
echo "not of the job's wall time."
echo
echo "| Phase | Duration | Image layers (compressed) |"
echo "| --- | ---: | ---: |"

awk -F'\t' '
function hms(s) {
    return sprintf("%dm %02ds", int(s / 60), s % 60)
}
function gib(b) {
    return b == "" ? "—" : sprintf("%.2f GiB", b / 1073741824)
}
{
    label[NR] = $1
    secs[NR] = $2
    bytes[NR] = $3
    total += $2
}
END {
    for (i = 1; i <= NR; i++) {
        printf "| %s | %s (%.0f%%) | %s |\n", \
            label[i], hms(secs[i]), total ? 100 * secs[i] / total : 0, gib(bytes[i])
    }
    printf "| **Total instrumented** | **%s** | |\n", hms(total)
}
' "$CI_PHASE_LOG"
