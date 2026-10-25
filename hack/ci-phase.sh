#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Record how long one phase of a CI job took, and how much image it produced.
#
# A job step's duration is already in the Actions UI, so this exists for what
# that cannot show: phases that share a step, and the image bytes that explain
# why a phase costs what it does. Issue 505 asks for pull, build, and import
# measured separately before anything is optimised, and the job currently
# reports a single number per step with the base-image pull hidden inside a
# build.
#
# Usage: ci-phase.sh <label> <start-epoch-seconds> [image]
#
# Writes one tab-separated row per call to $CI_PHASE_LOG. The rendering step
# reads that file; nothing here formats for display.

set -eu

if [ "$#" -lt 2 ]; then
    echo "usage: $0 <label> <start-epoch-seconds> [image]" >&2
    exit 2
fi

label="$1"
start="$2"
image="${3:-}"

: "${CI_PHASE_LOG:?CI_PHASE_LOG must name the file to append to}"

seconds=$(($(date +%s) - start))

# Absent rather than zero when there is no image, so the renderer can tell a
# phase that moved nothing from one whose size could not be read.
#
# Read from the registry rather than the Docker daemon, because images are
# pushed and never loaded there. The sum is of layer blobs only: a reference
# can resolve to an index, whose descriptor sizes are the manifests rather
# than the content, and a manifest also carries a config blob that is not a
# layer. Counting either would report a number that means nothing.
bytes=""
if [ -n "$image" ]; then
    raw=$(docker buildx imagetools inspect "$image" --raw 2>/dev/null) || raw=""
    # An index has to be resolved to the one platform this job builds before
    # there are any layers to add up.
    digest=$(printf '%s' "$raw" | jq -r '
        [.manifests[]? | select(.platform.os == "linux"
                                and .platform.architecture == "amd64")][0].digest // empty
    ' 2>/dev/null) || digest=""
    if [ -n "$digest" ]; then
        raw=$(docker buildx imagetools inspect "${image}@${digest}" --raw 2>/dev/null) || raw=""
    fi
    bytes=$(printf '%s' "$raw" | jq -r '
        [.layers[]?.size] | if length > 0 then add else empty end
    ' 2>/dev/null) || bytes=""
fi

printf '%s\t%s\t%s\n' "$label" "$seconds" "$bytes" >>"$CI_PHASE_LOG"
