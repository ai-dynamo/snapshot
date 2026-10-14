#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Extract a rootfs diff with the agent's bundled tar inside a placeholder older
# than AGENT_BASE_IMAGE.
#
# runtime.restoreTarCmd runs the bundled tar under the bundled loader so the
# placeholder's glibc never has to satisfy it. Nothing else proves that: the
# agent image's own post-condition resolves the bundle against the build
# stage's glibc, which always satisfies the loader, so a bundle shipped without
# its libc/ builds green there and fails only at restore.
#
# Usage: verify-bundle-glibc.sh <agent-image> [placeholder-image]

set -eu

AGENT_IMAGE=${1:?usage: verify-bundle-glibc.sh <agent-image> [placeholder-image]}
# Ubuntu 22.04 is glibc 2.35, below the GLIBC_2.38 the agent's tar needs.
PLACEHOLDER=${2:-ubuntu:22.04@sha256:5ec03bb3441e8b0bf3b4f9cd4629a1ae763010dc3035bb8da3ae6cf026486401}

workdir=$(mktemp -d)
cleanup() {
    [ -n "${cid:-}" ] && docker rm -f "$cid" >/dev/null 2>&1
    rm -rf "$workdir"
}
trap cleanup EXIT

cid=$(docker create "$AGENT_IMAGE")
docker cp "$cid:/snapshot-binaries" "$workdir/bundle"

# The assertions are on what the restore produces, not on the argv: the flags
# below mirror runtime.ApplyRootfsDiff, and a capability or xattr lost here
# means the extract-side mask regressed there.
docker run --rm --platform linux/amd64 -e DEBIAN_FRONTEND=noninteractive \
    -v "$workdir/bundle:/bundle:ro" "$PLACEHOLDER" sh -eu -c '
    apt-get update -qq >/dev/null
    apt-get install -y -qq attr libcap2-bin >/dev/null

    echo "placeholder: $(. /etc/os-release; echo "$PRETTY_NAME"), glibc $(ldd --version | head -1 | grep -o "[0-9]\+\.[0-9]\+$")"

    mkdir -p /src/app /dst
    echo payload > /src/app/prog
    chmod 0755 /src/app/prog
    setcap cap_net_raw+ep /src/app/prog
    setfattr -n user.snapshot.cached -v yes /src/app/prog
    # As runtime.CaptureRootfsDiff writes it.
    tar --xattrs --xattrs-exclude="trusted.*" -C /src -cf /diff.tar .

    if /bundle/tar --version >/dev/null 2>&1; then
        echo "NOTE: a direct exec worked, so this placeholder is no longer older than the agent" >&2
    fi

    /bundle/libc/ld-linux-x86-64.so.2 \
      --library-path /bundle/libc:/bundle/lib \
      /bundle/tar \
      --xattrs --xattrs-include="user.*" --xattrs-include="security.capability" \
      --numeric-owner --skip-old-files --blocking-factor=2048 \
      -C /dst -xf /diff.tar

    [ "$(cat /dst/app/prog)" = payload ]
    getfattr -d --absolute-names /dst/app/prog | grep -q "user.snapshot.cached=\"yes\""
    getcap /dst/app/prog | grep -q cap_net_raw
    echo "bundled tar extracted the rootfs diff with xattrs and capabilities intact"
'
