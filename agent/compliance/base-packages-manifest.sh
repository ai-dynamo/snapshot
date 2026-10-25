#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Writes the dpkg manifest of the image this runs in, to stdout.
#
# Runs inside the image and shells out to nothing: the agent Dockerfile
# executes it in a stage to check the committed baseline, and
# hack/capture-base-packages.sh pipes it through `docker run` to refresh that
# baseline. One copy, so the two cannot disagree about the format.

set -eu

raw=$(mktemp)
trap 'rm -f "$raw"' EXIT

dpkg-query -W -f='${Package}\t${Version}\t${source:Package}\t${source:Version}\n' >"$raw"

# A failed query must not produce an empty manifest that later looks like a
# base image with no packages.
test -s "$raw"

# Every row needs all four fields. collect-sources.sh keys source retrieval on
# the source package and version, and skips rows where they are empty -- so a
# malformed row would drop out of source collection silently, with no fetch and
# no entry in SKIPPED.txt.
awk -F'\t' '
  NF != 4 || $1 == "" || $2 == "" || $3 == "" || $4 == "" {
    printf "invalid manifest row %d: %s\n", NR, $0 > "/dev/stderr"; bad = 1
  }
  END { exit bad ? 1 : 0 }
' "$raw"

printf '%s\n' \
    '# Baseline dpkg manifest of the agent base image (AGENT_BASE_IMAGE).' \
    '#' \
    '# Captured with:' \
    '#   make capture-base-packages' \
    '#' \
    '# collect-sources.sh diffs the built image against this file to determine' \
    '# which packages the image adds, and fetches source for those.' \
    '#' \
    '# Format: <package>\t<version>\t<source-package>\t<source-version>'

# LC_ALL=C matches the sort collect-sources.sh uses inside the container, so
# `comm` sees both sides in the same collation.
LC_ALL=C sort "$raw"
