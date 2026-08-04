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
GO_LICENSES=${3:-/tmp/go-licenses.sh}

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

HEADER

    sh "$GO_LICENSES" "$VENDOR"
} > "$OUT"

echo "Wrote $OUT ($(wc -l < "$OUT") lines)"
