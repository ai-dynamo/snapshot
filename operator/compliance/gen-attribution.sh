#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the consolidated third-party attribution file at /legal/THIRD-PARTY.txt.
#
# The operator is distroless and adds no system packages, so the Go modules
# linked into its binaries are the whole of its third-party content.

set -eu

VENDOR=${1:-/sources/go/vendor}
OUT=${2:-/legal/THIRD-PARTY.txt}

mkdir -p "$(dirname "$OUT")"

{
    cat <<'HEADER'
================================================================================
THIRD-PARTY SOFTWARE NOTICES AND ATTRIBUTION
NVIDIA Dynamo Snapshot — operator
================================================================================

This file lists third-party open-source software redistributed in this
container image, together with the license text for each component.

SCOPE: this covers what this image adds on top of its base image. This image
adds no system packages; everything below is a Go module linked into its
binaries. Base-image components are attributed by that image.

CORRESPONDING SOURCE: upstream source for every module listed below ships
inside this image under /legal/source/. See /legal/source/README.txt.

================================================================================

SECTION 1 — GO MODULES (STATICALLY LINKED)
--------------------------------------------------------------------------------

HEADER

    if [ -f "$VENDOR/modules.txt" ]; then
        grep '^# ' "$VENDOR/modules.txt" | sed 's/^# /  /'
    fi

    echo
    echo "--------------------------------------------------------------------------------"
    echo "FULL LICENSE TEXT"
    echo "--------------------------------------------------------------------------------"
    find "$VENDOR" -type f \
        \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
        | sort | while read -r lf; do
        mod=$(dirname "${lf#"$VENDOR"/}")
        echo
        echo "================================================================================"
        echo "MODULE: $mod  ($(basename "$lf"))"
        echo "================================================================================"
        cat "$lf"
    done
} > "$OUT"

echo "Wrote $OUT ($(wc -l < "$OUT") lines)"
