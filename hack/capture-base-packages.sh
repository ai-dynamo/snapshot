#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Writes the package manifest of the agent base image to <output>, for the
# platform the agent is built for.
#
# The manifest itself is produced by agent/compliance/base-packages-manifest.sh
# running inside the image; this only arranges for it to run there. The agent
# Dockerfile runs the same script in a stage to check the committed baseline,
# so the two cannot disagree about how the manifest is produced.

set -eu

IMAGE=${1:?usage: capture-base-packages.sh <image> <output>}
OUT=${2:?usage: capture-base-packages.sh <image> <output>}
PLATFORM=${3:-linux/amd64}

MANIFEST_SH="$(dirname "$0")/../agent/compliance/base-packages-manifest.sh"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Piped in rather than mounted, so this works the same against a remote image
# and under a container runtime with no bind mounts.
docker run --rm --interactive --platform "$PLATFORM" --entrypoint sh "$IMAGE" -s \
    <"$MANIFEST_SH" >"$tmp"

mv "$tmp" "$OUT"
