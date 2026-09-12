#!/usr/bin/env sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exclude_file="$repo_root/hack/.notableofcontents"

find "$repo_root/docs/proposals" -type f -name '*.md' | sort | while IFS= read -r file; do
	rel_path=${file#"$repo_root"/}
	if grep -Fxq "$rel_path" "$exclude_file"; then
		printf '%s\n' "Excluded: $rel_path"
		continue
	fi

	mdtoc --inplace --max-depth=6 "$file"
	printf '%s\n' "Updated: $rel_path"
done
