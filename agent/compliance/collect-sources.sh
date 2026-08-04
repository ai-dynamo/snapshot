#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Fetches upstream source for every Debian package the agent image adds on top
# of its base image, so the corresponding source ships with the binaries.
#
# The delta is computed against base-packages.tsv rather than hardcoded, so a
# package added to the Dockerfile brings its source along automatically.

set -eu

BASELINE=${1:-/tmp/base-packages.tsv}
OUT=${2:-/sources/dpkg}

mkdir -p "$OUT"

# apt-get source needs deb-src, which Ubuntu's deb822 sources omit by default.
# The CUDA/NVIDIA repos publish no source and are left alone.
for f in /etc/apt/sources.list.d/*.sources; do
    [ -f "$f" ] || continue
    case "$(basename "$f")" in
        cuda*|nvidia*) continue ;;
    esac
    sed -i 's/^Types: deb$/Types: deb deb-src/' "$f"
done
if [ -f /etc/apt/sources.list ]; then
    sed -i 's/^deb \(.*\)$/deb \1\ndeb-src \1/' /etc/apt/sources.list
fi

apt-get update -qq

# Lines unique to the current manifest: both newly added packages and version
# upgrades of base packages, since we ship the upgraded version.
grep -v '^#' "$BASELINE" | sort > /tmp/baseline.sorted
dpkg-query -W -f='${Package}\t${Version}\t${source:Package}\n' | sort > /tmp/current.sorted
comm -13 /tmp/baseline.sorted /tmp/current.sorted > /tmp/delta.tsv

cut -f3 /tmp/delta.tsv | sort -u > /tmp/source-packages.txt
cp /tmp/delta.tsv "$OUT/DELTA.tsv"

echo "Source packages to fetch: $(wc -l < /tmp/source-packages.txt)"

: > "$OUT/SKIPPED.txt"
fetched=0
while read -r src; do
    [ -n "$src" ] || continue
    if (cd "$OUT" && apt-get source --only-source --download-only "$src" >/dev/null 2>&1); then
        fetched=$((fetched + 1))
    else
        echo "$src" >> "$OUT/SKIPPED.txt"
    fi
done < /tmp/source-packages.txt

echo "Fetched source for $fetched source package(s)"

# A missing source archive is a packaging bug, not a warning. Only the
# NVIDIA-proprietary CUDA packages legitimately have none.
if [ -s "$OUT/SKIPPED.txt" ]; then
    echo "No public source available for:"
    sed 's/^/  /' "$OUT/SKIPPED.txt"
    while read -r src; do
        case "$src" in
            cuda*|nvidia*|libcu*|libnv*|libnpp*|tensorrt*|nsight*) continue ;;
            *) echo "ERROR: no source available for '$src'" >&2; exit 1 ;;
        esac
    done < "$OUT/SKIPPED.txt"
fi
