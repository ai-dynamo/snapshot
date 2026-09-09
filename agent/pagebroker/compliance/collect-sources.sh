#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Fetches the corresponding source for protobuf, the only third-party component
# the PageBroker image adds on top of its distroless base. Run in the build
# stage, on the same libprotobuf-dev install that provided the libprotobuf.a
# linked into the daemon, so the shipped source is the source of the linked
# object rather than whatever the archive currently serves.

set -eu

OUT=${1:-/legal/source/protobuf}

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
version=$(dpkg-query -W -f='${source:Version}' libprotobuf-dev)
[ -n "$version" ] || { echo "ERROR: libprotobuf-dev is not installed" >&2; exit 1; }

mkdir -p "$OUT"
(cd "$OUT" && apt-get source --only-source --download-only "protobuf=$version")

printf '%s\n' "$version" > "$OUT/VERSION"
cp /usr/share/doc/libprotobuf-dev/copyright "$OUT/copyright"

echo "Fetched protobuf source $version"
