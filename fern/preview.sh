#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Local preview of the docs structure. Fern reads Markdown as MDX, and the repo
# docs are written for GitHub, so this copies docs/ and fern/ to a temporary
# folder, rewrites the few things MDX rejects in the copy, and runs Fern there.
# The files in the repo are not changed. In dev mode the copy is refreshed
# whenever a file under docs/ or fern/ changes.
#
# Usage: fern/preview.sh [dev|check]   (default: dev)

set -euo pipefail

mode="${1:-dev}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
pids=()
cleanup() {
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

# In the copy only:
#   <!-- ... -->   becomes  {/* ... */}   (HTML comments are not valid MDX)
#   <img ...>      becomes  <img ... />   (MDX needs void tags closed)
#   <source>, <br>, <hr> are closed the same way
#   > [!NOTE] ...  becomes  <Note> ... </Note>   (GitHub alerts are not Fern callouts)
sync_copy() {
  rsync -a --delete --exclude design "$root/docs/" "$work/docs/"
  rsync -a --delete "$root/fern/" "$work/fern/"
  find "$work/docs" -name '*.md' -print0 | xargs -0 perl -0pi -e '
    s/<!--(.*?)-->/{\/*$1*\/}/gs;
    s/<(img|source|br|hr)((?:\s[^<>]*?)?)(?<!\/)>/<$1$2 \/>/g;
    s{^> \[!(NOTE|TIP|WARNING|IMPORTANT|CAUTION)\][ \t]*\n((?:>.*(?:\n|\z))*)}{
      my ($t, $b) = ($1, $2);
      $b =~ s/^> ?//mg;
      my %c = (NOTE => "Note", TIP => "Tip", WARNING => "Warning", IMPORTANT => "Warning", CAUTION => "Warning");
      "<$c{$t}>\n$b<\/$c{$t}>\n"
    }gem;
  '
}

# A fingerprint of every file's path, size, and modification time.
fingerprint() {
  find "$root/docs" "$root/fern" -type f -not -path '*/design/*' \
    -exec stat -f '%N %z %m' {} + 2>/dev/null | sort | shasum || true
}

mkdir -p "$work/docs" "$work/fern"
sync_copy

case "$mode" in
  check) cd "$work/fern" && fern check --warnings ;;
  dev)
    (
      last="$(fingerprint)"
      while sleep 2; do
        now="$(fingerprint)" || continue
        if [ "$now" != "$last" ]; then
          last="$now"
          sync_copy
          echo "preview: refreshed from the repository"
        fi
      done
    ) &
    pids+=("$!")
    cd "$work/fern" && fern docs dev
    ;;
  *) echo "usage: $0 [dev|check]" >&2; exit 1 ;;
esac
