#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build the statically linked GNU tar used for restore-time rootfs-diff
# extraction, assert it is fully static, and smoke-test the exact invocation
# runtime.ApplyRootfsDiff uses. Installs the binary and its license text to
# /out. Run from the unpacked GNU tar source directory.
#
# Usage: build-gnu-tar.sh
set -eu

# FORCE_UNSAFE_CONFIGURE: GNU tar's configure refuses to run as root otherwise.
FORCE_UNSAFE_CONFIGURE=1 LDFLAGS=-static ./configure \
    --disable-nls \
    --without-posix-acls \
    --without-selinux
make -j"$(nproc)"

mkdir -p /out
cp src/tar /out/tar
cp COPYING /out/COPYING
chmod 0755 /out/tar
strip /out/tar

# A static binary has neither an ELF interpreter nor NEEDED entries; either
# one means the placeholder's userspace would leak back into restore.
if readelf -l /out/tar | grep -q 'INTERP'; then
    echo "ERROR: restore tar has a dynamic ELF interpreter" >&2
    exit 1
fi
if readelf -d /out/tar | grep -q '(NEEDED)'; then
    echo "ERROR: restore tar has shared-library dependencies" >&2
    exit 1
fi

# Smoke test mirroring the restore invocation in runtime.ApplyRootfsDiff —
# keep these flags in sync with that call site.
mkdir -p /tmp/tar-smoke/source /tmp/tar-smoke/target
echo preserved > /tmp/tar-smoke/target/existing
echo replaced > /tmp/tar-smoke/source/existing
echo created > /tmp/tar-smoke/source/created
setfattr -n user.snapshot-smoke -v smoke /tmp/tar-smoke/source/created
/out/tar --xattrs -C /tmp/tar-smoke/source -cf /tmp/tar-smoke/archive.tar .
/out/tar --skip-old-files --blocking-factor=2048 \
    --xattrs --xattrs-include='user.*' --xattrs-include='security.*' \
    -C /tmp/tar-smoke/target -xf /tmp/tar-smoke/archive.tar
grep -qx preserved /tmp/tar-smoke/target/existing
grep -qx created /tmp/tar-smoke/target/created
test "$(getfattr --only-values -n user.snapshot-smoke /tmp/tar-smoke/target/created)" = smoke
