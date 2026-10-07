#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Fetch corresponding source for an installed image dependency. The default
# remains protobuf, whose development package provides the linked library.

set -eu

OUT=${1:-/legal/source/protobuf}
PACKAGE=${2:-libprotobuf-dev}

# apt-get source needs deb-src, which Ubuntu's deb822 sources omit by default.
for f in /etc/apt/sources.list.d/*.sources; do
    [ -f "$f" ] || continue
    sed -i 's/^Types: deb$/Types: deb deb-src/' "$f"
done
if [ -f /etc/apt/sources.list ]; then
    sed -i 's/^deb \(.*\)$/deb \1\ndeb-src \1/' /etc/apt/sources.list
fi

apt-get update -qq

# Pin to the source version of the installed development package. Without the
# version, apt fetches the archive's current source, which may differ from the
# code we linked.
if ! version=$(dpkg-query -W -f='${source:Version}' "$PACKAGE") ||
   ! source=$(dpkg-query -W -f='${source:Package}' "$PACKAGE") ||
   [ -z "$version" ] || [ -z "$source" ]; then
    echo "ERROR: $PACKAGE is not installed" >&2
    exit 1
fi

mkdir -p "$OUT"
(cd "$OUT" && apt-get source --only-source --download-only "$source=$version")

printf '%s\n' "$version" > "$OUT/VERSION"
cp "/usr/share/doc/$PACKAGE/copyright" "$OUT/copyright"

echo "Fetched $source source $version"
