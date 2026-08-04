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

# Fields: binary package, binary version, source package, source version.
QUERY='${Package}\t${Version}\t${source:Package}\t${source:Version}\n'

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
# LC_ALL=C throughout: comm requires both inputs in the same collation, and
# package names contain '-', '.' and '+', which locale-aware collations order
# differently from byte order. A mismatch would silently skew the delta.
grep -v '^#' "$BASELINE" | LC_ALL=C sort > /tmp/baseline.sorted
dpkg-query -W -f="$QUERY" | LC_ALL=C sort > /tmp/current.sorted
LC_ALL=C comm -13 /tmp/baseline.sorted /tmp/current.sorted > /tmp/delta.tsv

# Source retrieval is keyed on the source package and version below, and rows
# missing either would be skipped without a fetch or a SKIPPED.txt entry. Fail
# instead of shipping an image whose source set is quietly incomplete.
awk -F'\t' '
  NF != 4 || $1 == "" || $2 == "" || $3 == "" || $4 == "" {
    printf "invalid delta row %d: %s\n", NR, $0 > "/dev/stderr"; bad = 1
  }
  END { exit bad ? 1 : 0 }
' /tmp/delta.tsv

cp /tmp/delta.tsv "$OUT/DELTA.tsv"

# One row per (source package, source version), keeping a representative binary
# package so a failure can be traced back to the repository it came from.
awk -F'\t' '!seen[$3"="$4]++ { print $3"\t"$4"\t"$1 }' /tmp/delta.tsv > /tmp/source-packages.tsv

echo "Source packages to fetch: $(wc -l < /tmp/source-packages.tsv)"

: > "$OUT/SKIPPED.txt"
fetched=0
failed=0

while IFS="$(printf '\t')" read -r src srcver bin; do
    [ -n "$src" ] || continue

    # Pin to the source version the installed binary was built from. Without
    # the version, apt fetches whatever is current in the archive, which may
    # not be the source corresponding to the binary we ship.
    if (cd "$OUT" && apt-get source --only-source --download-only "$src=$srcver" >/dev/null 2>&1); then
        fetched=$((fetched + 1))
        continue
    fi

    # Classify the failure by repository origin rather than by package name.
    # A name-prefix allowlist would wave through unrelated packages that happen
    # to share a prefix, and would mask transient or repository errors.
    if [ -n "$(apt-cache showsrc "$src" 2>/dev/null)" ]; then
        reason="apt knows source for $src=$srcver but the download failed"
        failed=$((failed + 1))
    else
        origin=$(apt-cache policy "$bin" 2>/dev/null \
                 | awk '/https?:\/\//{ for(i=1;i<=NF;i++) if ($i ~ /^https?:\/\//) { print $i; exit } }')
        case "$origin" in
            *nvidia.com*|*nvidia.cn*)
                reason="no source published; NVIDIA repository ($origin)"
                ;;
            *)
                reason="no source record; origin ${origin:-unknown}"
                failed=$((failed + 1))
                ;;
        esac
    fi
    printf '%s\t%s\t%s\t%s\n' "$src" "$srcver" "$bin" "$reason" >> "$OUT/SKIPPED.txt"
done < /tmp/source-packages.tsv

echo "Fetched source for $fetched source package(s)"

if [ -s "$OUT/SKIPPED.txt" ]; then
    echo "Source not fetched:"
    awk -F'\t' '{ printf "  %s=%s: %s\n", $1, $2, $4 }' "$OUT/SKIPPED.txt"
fi

# Anything not explained by an NVIDIA repository is a real gap, not a warning.
if [ "$failed" -gt 0 ]; then
    echo "ERROR: $failed source package(s) could not be fetched and are not NVIDIA-published" >&2
    exit 1
fi
