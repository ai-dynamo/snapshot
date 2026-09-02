#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build a statically linked criu, assert it is fully static, and verify its
# CRIU-internal symbols required by the restore-time plugins (CUDA,
# inet-remap) are still resolvable via dlopen. Installs into DESTDIR.
#
# criu's optional dependencies (gnutls, nftables, libbsd, libselinux, libbpf,
# libdrm) are feature-tested at build time and silently dropped if their
# headers are absent — this build intentionally omits them so the static
# link only has to satisfy the mandatory deps (protobuf-c, libnl, libcap,
# libnet, libuuid).
#
# A global LDFLAGS=-static breaks criu's parasite/restorer PIE blobs (they
# need non-standard freestanding linking, not a normal static executable),
# so staticness is scoped to just the final criu binary's link line via a
# patch to criu/Makefile, applied by this script.
#
# Usage: build-static-criu.sh <criu-source-dir> <destdir>
set -eu

SRC="$1"
DESTDIR="$2"

LINK_LINE='	$(Q) $(CC) $(CFLAGS) $^ $(LDFLAGS) $(LIBS) $(WRAPFLAGS) $(GMONLDOPT) -rdynamic -o $@'
STATIC_LINK_LINE="	\$(Q) \$(CC) \$(CFLAGS) \$^ \$(LDFLAGS) -static -Wl,--export-dynamic-symbol='*' \$(LIBS) \$(WRAPFLAGS) \$(GMONLDOPT) -rdynamic -o \$@"

MAKEFILE="${SRC}/criu/Makefile"
if ! grep -qF "${LINK_LINE}" "${MAKEFILE}"; then
    echo "ERROR: criu/Makefile final-link recipe has changed upstream; update build-static-criu.sh's patch to match" >&2
    exit 1
fi
# Portable in-place replace: write to a temp file, then move over the original.
awk -v old="${LINK_LINE}" -v new="${STATIC_LINK_LINE}" \
    '{ if ($0 == old) print new; else print }' "${MAKEFILE}" > "${MAKEFILE}.tmp"
mv "${MAKEFILE}.tmp" "${MAKEFILE}"

cd "${SRC}"
make -j"$(nproc)"
make DESTDIR="${DESTDIR}" install-criu install-lib install-cuda_plugin

CRIU_BIN="${DESTDIR}/usr/local/sbin/criu"

# A static binary has neither an ELF interpreter nor NEEDED entries; either
# one means the placeholder's userspace would leak back into restore.
if readelf -l "${CRIU_BIN}" | grep -q 'INTERP'; then
    echo "ERROR: restore criu has a dynamic ELF interpreter" >&2
    exit 1
fi
if readelf -d "${CRIU_BIN}" | grep -q '(NEEDED)'; then
    echo "ERROR: restore criu has shared-library dependencies" >&2
    exit 1
fi

# The CUDA and inet-remap plugins are dlopen'd by criu at runtime and call
# back into criu-internal symbols (e.g. print_on_level, compel_wait_task).
# --export-dynamic-symbol='*' must keep those resolvable even though the
# binary itself is static; assert a representative symbol survived.
if ! readelf --dyn-syms "${CRIU_BIN}" | grep -q 'compel_wait_task'; then
    echo "ERROR: static criu is missing exported symbols required by dlopen'd plugins" >&2
    exit 1
fi

"${CRIU_BIN}" --version
