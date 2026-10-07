#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the consolidated third-party attribution file at /legal/THIRD-PARTY.txt.
#
# Read notices beside the matching source. The protobuf directory remains
# the first argument. Additional dependency directories follow the output path.

set -eu

SOURCE=${1:-/legal/source/protobuf}
OUT=${2:-/legal/THIRD-PARTY.txt}

[ -f "$SOURCE/copyright" ] || { echo "ERROR: $SOURCE/copyright not found" >&2; exit 1; }
[ -f "$SOURCE/VERSION" ] || { echo "ERROR: $SOURCE/VERSION not found" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"

{
    cat <<'HEADER'
================================================================================
THIRD-PARTY SOFTWARE NOTICES AND ATTRIBUTION
NVIDIA Dynamo Snapshot — PageBroker
================================================================================

This file lists third-party open-source software redistributed in this
container image, together with the license text for each component.

SCOPE: this covers what this image adds on top of its base image. This image
includes protobuf, linked statically into /usr/local/bin/pagebroker. Additional components are listed
below when included. Base-image components are attributed by that image.

CORRESPONDING SOURCE: upstream source for the component listed below ships
inside this image under /legal/source/. See /legal/source/README.txt.

================================================================================

HEADER

    printf '================================================================================\n'
    printf 'COMPONENT: protobuf\n'
    printf 'VERSION:   %s (Debian source version)\n' "$(cat "$SOURCE/VERSION")"
    printf 'SOURCE:    /legal/source/protobuf/\n'
    printf '================================================================================\n\n'
    cat "$SOURCE/copyright"
    if [ "$#" -gt 2 ]; then
        shift 2
        for component in "$@"; do
            printf '\n================================================================================\n'
            printf 'COMPONENT: %s\n' "$(basename "$component")"
            printf 'VERSION: %s\nSOURCE: %s/\n\n' "$(cat "$component/VERSION")" "$component"
            cat "$component/copyright"
            if [ -f "$component/THIRD-PARTY-NOTICES.txt" ]; then
                cat "$component/THIRD-PARTY-NOTICES.txt"
            fi
        done
    fi
} > "$OUT"

echo "Wrote $OUT"
