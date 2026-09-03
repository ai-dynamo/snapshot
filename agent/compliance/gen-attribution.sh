#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the consolidated third-party attribution file at /legal/THIRD-PARTY.txt
# from the package delta and the vendored Go modules, so it cannot drift from
# what is actually installed.
#
# Components inherited from the base image are attributed by that image and are
# not repeated here.

set -eu

DELTA=${1:-/sources/dpkg/DELTA.tsv}
VENDOR=${2:-/sources/go/vendor}
OUT=${3:-/legal/THIRD-PARTY.txt}
SKIPPED=${4:-/sources/dpkg/SKIPPED.txt}
GO_LICENSES=${5:-/tmp/go-licenses.sh}

mkdir -p "$(dirname "$OUT")"

{
    cat <<'HEADER'
================================================================================
THIRD-PARTY SOFTWARE NOTICES AND ATTRIBUTION
NVIDIA Dynamo Snapshot
================================================================================

This file lists third-party open-source software redistributed in this
container image, together with the license text for each component.

SCOPE: this covers the components this image adds on top of its base image.
Components belonging to the base image are attributed by that image.

CORRESPONDING SOURCE: upstream source ships inside this image under
/legal/source/. Components marked [NO SOURCE ARCHIVE] below are the exception
and are annotated with the reason. See /legal/source/README.txt.

================================================================================
HEADER

    if [ -f "$DELTA" ]; then
        echo
        echo "SYSTEM (DEBIAN/UBUNTU) PACKAGES"
        echo "--------------------------------------------------------------------------------"
        echo
        while IFS="$(printf '\t')" read -r pkg ver src srcver; do
            [ -n "$pkg" ] || continue
            note=""
            if [ -f "$SKIPPED" ] && awk -F'\t' -v s="$src" -v v="$srcver" '$1==s && $2==v{found=1} END{exit !found}' "$SKIPPED"; then
                note="  [NO SOURCE ARCHIVE: $(awk -F'\t' -v s="$src" -v v="$srcver" '$1==s && $2==v{print $4; exit}' "$SKIPPED")]"
            fi
            echo "  $pkg ($ver)  [source package: $src]$note"
        done < "$DELTA"

        echo
        echo "--------------------------------------------------------------------------------"
        echo "FULL LICENSE TEXT — SYSTEM PACKAGES"
        echo "--------------------------------------------------------------------------------"
        while IFS="$(printf '\t')" read -r pkg ver src srcver; do
            [ -n "$pkg" ] || continue
            cp_file="/usr/share/doc/$pkg/copyright"
            echo
            echo "================================================================================"
            echo "PACKAGE: $pkg ($ver)"
            if [ -f "$SKIPPED" ] && awk -F'\t' -v s="$src" -v v="$srcver" '$1==s && $2==v{found=1} END{exit !found}' "$SKIPPED"; then
                echo "NO SOURCE ARCHIVE: $(awk -F'\t' -v s="$src" -v v="$srcver" '$1==s && $2==v{print $4; exit}' "$SKIPPED")"
            fi
            echo "================================================================================"
            if [ -f "$cp_file" ]; then
                cat "$cp_file"
            else
                echo "(no copyright file in this package)"
            fi
        done < "$DELTA"
    fi

    sh "$GO_LICENSES" "$VENDOR"

    # Fold in the license texts the Dockerfile stages separately, so this file
    # is self-contained.
    for extra in /legal/CRIU/COPYING /legal/cuda-checkpoint/LICENSE /legal/gnu-tar/COPYING; do
        [ -f "$extra" ] || continue
        echo
        echo "================================================================================"
        echo "COMPONENT: $(echo "$extra" | cut -d/ -f3)"
        echo "================================================================================"
        cat "$extra"
    done
} > "$OUT"

echo "Wrote $OUT ($(wc -l < "$OUT") lines)"
