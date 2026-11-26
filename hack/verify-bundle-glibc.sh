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
# Usage: verify-bundle-glibc.sh <agent-image-or-bundle-dir> [placeholder-image]
#
# A directory is the agent Dockerfile's bundle-export stage, already extracted.
# An image is read with `docker create`, which needs it in the local daemon --
# fine locally, but CI pushes the agent image to a registry rather than loading
# it, so CI passes the directory.
#
# The placeholder needs GNU tar and python3.

set -eu

BUNDLE_SRC=${1:?usage: verify-bundle-glibc.sh <agent-image-or-bundle-dir> [placeholder-image]}
# The Python image on Debian 12 has glibc 2.36, below the GLIBC_2.38 the agent's
# tar needs. Its python3 sets and reads the xattrs, so nothing is installed
# from a package mirror that can be down or mid-sync.
PLACEHOLDER=${2:-python:3.12-slim-bookworm@sha256:44cf1793efa7c7a476bb0f2cff81443f4a072c90c1cb127e2924933daa3e8c64}
# cap_net_raw+ep, as the bytes setcap writes for it.
CAP_NET_RAW_EP=0100000200200000000000000000000000000000

workdir=$(mktemp -d)
cleanup() {
    [ -n "${cid:-}" ] && docker rm -f "$cid" >/dev/null 2>&1
    rm -rf "$workdir"
}
trap cleanup EXIT

if [ -d "$BUNDLE_SRC" ]; then
    cp -a "$BUNDLE_SRC" "$workdir/bundle"
else
    cid=$(docker create "$BUNDLE_SRC")
    docker cp "$cid:/snapshot-binaries" "$workdir/bundle"
fi

# Whichever way it arrived, an empty bundle must not read as a clean pass.
[ -e "$workdir/bundle/tar" ] || {
    echo "no bundled tar under $BUNDLE_SRC" >&2
    exit 1
}

# The bundle names its own architecture: it carries the loader of the platform
# it was built for.
if [ -e "$workdir/bundle/libc/ld-linux-aarch64.so.1" ]; then
    arch=arm64
    loader=ld-linux-aarch64.so.1
elif [ -e "$workdir/bundle/libc/ld-linux-x86-64.so.2" ]; then
    arch=amd64
    loader=ld-linux-x86-64.so.2
else
    echo "no known loader under $BUNDLE_SRC/libc" >&2
    exit 1
fi

# The assertions are on what the restore produces, not on the argv: the flags
# below mirror runtime.ApplyRootfsDiff, and a capability or xattr lost here
# means the extract-side mask regressed there.
docker run --rm --network none --platform "linux/$arch" \
    -e LOADER="$loader" -e CAPABILITY="$CAP_NET_RAW_EP" \
    -v "$workdir/bundle:/bundle:ro" "$PLACEHOLDER" sh -eu -c '
    echo "placeholder: $(. /etc/os-release; echo "$PRETTY_NAME"), glibc $(ldd --version | head -1 | grep -o "[0-9]\+\.[0-9]\+$")"

    mkdir -p /src/app /dst
    echo payload > /src/app/prog
    chmod 0755 /src/app/prog
    python3 - /src/app/prog <<"EOF"
import os, sys
os.setxattr(sys.argv[1], "security.capability", bytes.fromhex(os.environ["CAPABILITY"]))
os.setxattr(sys.argv[1], "user.snapshot.cached", b"yes")
EOF
    # As runtime.CaptureRootfsDiff writes it.
    tar --xattrs --xattrs-exclude="trusted.*" -C /src -cf /diff.tar .

    if /bundle/tar --version >/dev/null 2>&1; then
        echo "NOTE: a direct exec worked, so this placeholder is no longer older than the agent" >&2
    fi

    "/bundle/libc/$LOADER" \
      --library-path /bundle/libc:/bundle/lib \
      /bundle/tar \
      --xattrs --xattrs-include="user.*" --xattrs-include="security.capability" \
      --numeric-owner --skip-old-files --blocking-factor=2048 \
      -C /dst -xf /diff.tar

    [ "$(cat /dst/app/prog)" = payload ]
    python3 - /dst/app/prog <<"EOF"
import os, sys
assert os.getxattr(sys.argv[1], "user.snapshot.cached") == b"yes"
assert os.getxattr(sys.argv[1], "security.capability") == bytes.fromhex(os.environ["CAPABILITY"])
EOF
    echo "bundled tar extracted the rootfs diff with xattrs and capabilities intact"
'
