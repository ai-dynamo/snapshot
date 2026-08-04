#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the consolidated third-party attribution file shipped at
# /legal/THIRD-PARTY.txt.
#
# Covers everything the image adds on top of its NGC base:
#   - Debian packages in the delta (full copyright text from the package itself)
#   - Go modules statically linked into the NVIDIA binaries (LICENSE from vendor)
#   - CRIU and cuda-checkpoint, whose license texts are copied in separately
#
# Base-image components are deliberately excluded and called out as such: they
# are attributed by the NGC base image, not by us.

set -eu

DELTA=${1:-/sources/dpkg/DELTA.tsv}
VENDOR=${2:-/sources/go/vendor}
OUT=${3:-/legal/THIRD-PARTY.txt}

mkdir -p "$(dirname "$OUT")"

{
    cat <<'HEADER'
================================================================================
THIRD-PARTY SOFTWARE NOTICES AND ATTRIBUTION
NVIDIA Dynamo Snapshot
================================================================================

This file lists third-party open-source software redistributed in this
container image, together with the license text for each component.

SCOPE: this covers the components this image adds on top of its NVIDIA base
container. Components belonging to the base image itself are attributed by
that base image and are not repeated here.

CORRESPONDING SOURCE: upstream source for every component listed below ships
inside this image under /legal/source/. See /legal/source/README.txt.

================================================================================
HEADER

    if [ -f "$DELTA" ]; then
        echo
        echo "SECTION 1 — SYSTEM (DEBIAN/UBUNTU) PACKAGES"
        echo "--------------------------------------------------------------------------------"
        echo
        while IFS="$(printf '\t')" read -r pkg ver src; do
            [ -n "$pkg" ] || continue
            echo "  $pkg ($ver)  [source package: $src]"
        done < "$DELTA"

        echo
        echo "--------------------------------------------------------------------------------"
        echo "FULL LICENSE TEXT — SYSTEM PACKAGES"
        echo "--------------------------------------------------------------------------------"
        while IFS="$(printf '\t')" read -r pkg ver src; do
            [ -n "$pkg" ] || continue
            cp_file="/usr/share/doc/$pkg/copyright"
            echo
            echo "================================================================================"
            echo "PACKAGE: $pkg ($ver)"
            echo "================================================================================"
            if [ -f "$cp_file" ]; then
                cat "$cp_file"
            else
                echo "(copyright file not present; see source package '$src' under /legal/source/)"
            fi
        done < "$DELTA"
    fi

    if [ -d "$VENDOR" ]; then
        echo
        echo "================================================================================"
        echo "SECTION 2 — GO MODULES (STATICALLY LINKED)"
        echo "--------------------------------------------------------------------------------"
        echo
        # vendor/modules.txt lists the exact module set and versions the build used.
        if [ -f "$VENDOR/modules.txt" ]; then
            grep '^# ' "$VENDOR/modules.txt" | sed 's/^# /  /'
        fi

        echo
        echo "--------------------------------------------------------------------------------"
        echo "FULL LICENSE TEXT — GO MODULES"
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
    fi

    # CRIU and cuda-checkpoint license texts are staged into the image by the
    # Dockerfile; fold them into the consolidated file so /legal/THIRD-PARTY.txt
    # is genuinely complete rather than pointing elsewhere.
    for extra in /legal/CRIU/COPYING /legal/cuda-checkpoint/LICENSE; do
        [ -f "$extra" ] || continue
        echo
        echo "================================================================================"
        echo "COMPONENT: $(echo "$extra" | cut -d/ -f3)"
        echo "================================================================================"
        cat "$extra"
    done
} > "$OUT"

echo "Wrote $OUT ($(wc -l < "$OUT") lines)"
