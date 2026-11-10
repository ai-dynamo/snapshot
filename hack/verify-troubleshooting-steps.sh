#!/usr/bin/env sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Every checkpoint and restore step the agent can name in a failure must have
# an entry in the troubleshooting guide. The step list is read from the Go
# source so the two cannot drift apart silently.

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
steps_file="$repo_root/agent/internal/executor/step.go"
doc_checkpoints="$repo_root/docs/troubleshooting/checkpoints.md"
doc_restores="$repo_root/docs/troubleshooting/restores.md"
missing_file=$(mktemp)
trap 'rm -f "$missing_file"' EXIT

sed -n 's/^[[:space:]]*Step[A-Za-z0-9]*[[:space:]]*Step = "\(.*\)"[[:space:]]*\(\/\/.*\)\{0,1\}$/\1/p' "$steps_file" | while IFS= read -r step; do
	if ! grep -Fxq "### $step" "$doc_checkpoints" "$doc_restores"; then
		printf '%s\n' "$step" >> "$missing_file"
	fi
done

if [ -s "$missing_file" ]; then
	printf '%s\n' "docs/troubleshooting/checkpoints.md and docs/troubleshooting/restores.md have no entry for these steps from agent/internal/executor/step.go:" >&2
	sed 's/^/  ### /' "$missing_file" >&2
	exit 1
fi
