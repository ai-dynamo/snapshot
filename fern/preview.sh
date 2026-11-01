#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Local preview of the docs structure. Fern reads Markdown as MDX, and the repo
# docs are written for GitHub, so this copies README.md, docs/, and fern/ to a
# temporary folder, rewrites the few things MDX rejects in the copy, and runs
# Fern there. The files in the repo are not changed.
#
# Usage: fern/preview.sh [dev|check]   (default: dev)

set -euo pipefail

mode="${1:-dev}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp "$root/README.md" "$work/"
cp -R "$root/docs" "$root/fern" "$work/"
rm -rf "$work/docs/proposals"

# In the copy only:
#   <!-- ... -->   becomes  {/* ... */}   (HTML comments are not valid MDX)
#   <img ...>      becomes  <img ... />   (MDX needs void tags closed)
#   <source>, <br>, <hr> are closed the same way
find "$work" -name '*.md' -print0 | xargs -0 perl -0pi -e '
  s/<!--(.*?)-->/{\/*$1*\/}/gs;
  s/<(img|source|br|hr)((?:\s[^<>]*?)?)(?<!\/)>/<$1$2 \/>/g;
'

cd "$work/fern"
case "$mode" in
  dev) fern docs dev ;;
  check) fern check --warnings ;;
  *) echo "usage: $0 [dev|check]" >&2; exit 1 ;;
esac
