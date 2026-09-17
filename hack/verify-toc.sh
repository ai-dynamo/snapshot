#!/usr/bin/env sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exclude_file="$repo_root/hack/.notableofcontents"
stale_file=$(mktemp)
trap 'rm -f "$stale_file"' EXIT

find "$repo_root/docs/proposals" -type f -name '*.md' | sort | while IFS= read -r file; do
	rel_path=${file#"$repo_root"/}
	if ! grep -Fxq "$rel_path" "$exclude_file" && ! mdtoc --inplace --max-depth=6 --dryrun "$file"; then
		printf '%s\n' "$rel_path" >> "$stale_file"
	fi
done

if [ -s "$stale_file" ]; then
	printf '%s\n' 'Table of contents is not up to date. Run `make update-toc`.' >&2
	cat "$stale_file" >&2
	exit 1
fi
