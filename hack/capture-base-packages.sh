#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Writes the package manifest of the agent base image to <output>, for the
# platform the agent is built for.
#
# Shared by `make capture-base-packages` (writes the committed baseline) and
# `make verify-base-packages` (writes a temporary copy to diff against it), so
# the two cannot disagree about how the manifest is produced.

set -eu

IMAGE=${1:?usage: capture-base-packages.sh <image> <output>}
OUT=${2:?usage: capture-base-packages.sh <image> <output>}
PLATFORM=${3:-linux/amd64}

tmp=$(mktemp)
raw=$(mktemp)
trap 'rm -f "$tmp" "$raw"' EXIT

printf '%s\n' \
  '# Baseline dpkg manifest of the agent base image (AGENT_BASE_IMAGE).' \
  '#' \
  '# Captured with:' \
  '#   make capture-base-packages' \
  '#' \
  '# collect-sources.sh diffs the built image against this file to determine' \
  '# which packages the image adds, and fetches source for those.' \
  '#' \
  '# Format: <package>\t<version>\t<source-package>\t<source-version>' \
  > "$tmp"

docker run --rm --platform "$PLATFORM" --entrypoint dpkg-query "$IMAGE" \
  -W -f='${Package}\t${Version}\t${source:Package}\t${source:Version}\n' > "$raw"

# A failed docker run must not produce an empty manifest that later looks like
# a base image with no packages.
test -s "$raw"

# Every row needs all four fields. collect-sources.sh keys source retrieval on
# the source package and version, and skips rows where they are empty — so a
# malformed row would drop out of source collection silently, with no fetch and
# no entry in SKIPPED.txt.
awk -F'\t' '
  NF != 4 || $1 == "" || $2 == "" || $3 == "" || $4 == "" {
    printf "invalid manifest row %d: %s\n", NR, $0 > "/dev/stderr"; bad = 1
  }
  END { exit bad ? 1 : 0 }
' "$raw"

# LC_ALL=C matches the sort collect-sources.sh uses inside the container, so
# `comm` sees both sides in the same collation.
LC_ALL=C sort "$raw" >> "$tmp"

mv "$tmp" "$OUT"
