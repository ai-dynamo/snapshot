#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the consolidated third-party attribution file at /legal/THIRD-PARTY.txt.
#
# The PageBroker image is distroless and adds no system packages, so the
# statically linked protobuf is the whole of its third-party content. The
# license text is read from the copyright file collect-sources.sh placed next
# to the corresponding source, so attribution and source always describe the
# same version.

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
adds no system packages; the only third-party component is protobuf, linked
statically into /usr/local/bin/pagebroker. Base-image components are
attributed by that image.

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
} > "$OUT"

echo "Wrote $OUT"
