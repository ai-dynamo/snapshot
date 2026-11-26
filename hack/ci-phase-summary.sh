#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Render ci-phase.sh rows. The main log contains non-overlapping phases;
# concurrent image builds go in its .builds companion and are shown separately.
# Uninstrumented work is absent from the total.
#
# Usage: ci-phase-summary.sh [cache-state]

set -eu

: "${CI_PHASE_LOG:?CI_PHASE_LOG must name the file to read}"

cache_state="${1:-unknown}"

set -- /dev/null
if [ -s "$CI_PHASE_LOG" ]; then
    set -- "$CI_PHASE_LOG"
fi
if [ -s "${CI_PHASE_LOG}.builds" ]; then
    set -- "$@" "${CI_PHASE_LOG}.builds"
fi

awk -F'\t' -v cache_state="$cache_state" -v builds="${CI_PHASE_LOG}.builds" '
function hms(s) {
    return sprintf("%dm %02ds", int(s / 60), s % 60)
}
function gib(b) {
    return b == "" ? "—" : sprintf("%.2f GiB", b / 1073741824)
}
FILENAME == builds {
    build_label[++build_count] = $1
    build_secs[build_count] = $2
    build_bytes[build_count] = $3
    next
}
{
    label[++count] = $1
    secs[count] = $2
    bytes[count] = $3
    total += $2
}
END {
    print "### Job phases\n"
    if (!count) {
        print "No phases were recorded; the job stopped before the first one finished."
    } else {
        printf "Cache: %s. Percentages are of the instrumented phases below,\n", cache_state
        print "not of the job\047s wall time.\n"
        print "| Phase | Duration | Image layers (compressed) |"
        print "| --- | ---: | ---: |"
        for (i = 1; i <= count; i++) {
            printf "| %s | %s (%.0f%%) | %s |\n", \
                label[i], hms(secs[i]), total ? 100 * secs[i] / total : 0, gib(bytes[i])
        }
        printf "| **Total instrumented** | **%s** | |\n", hms(total)
    }
    if (build_count) {
        print "\n### Concurrent image builds\n"
        print "These builds overlap within the preparation phase. Their durations are not added to the total above.\n"
        print "| Image build | Duration | Image layers (compressed) |"
        print "| --- | ---: | ---: |"
        for (i = 1; i <= build_count; i++) {
            printf "| %s | %s | %s |\n", build_label[i], hms(build_secs[i]), gib(build_bytes[i])
        }
    }
}
' "$@"
