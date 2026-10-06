#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Download and verify the pinned GNU tar source tarball, keep a pristine copy
# at /gnu-tar-source.tar.xz for the image's corresponding-source staging, and
# unpack the source into the current directory for the build.
#
# Usage: fetch-gnu-tar.sh <version> <sha256>
set -eu

VERSION="$1"
SHA256="$2"

curl -fsSLo gnu-tar.tar.xz "https://ftp.gnu.org/gnu/tar/tar-${VERSION}.tar.xz"
echo "${SHA256}  gnu-tar.tar.xz" | sha256sum -c -
cp gnu-tar.tar.xz /gnu-tar-source.tar.xz
xz -d gnu-tar.tar.xz
tar -xf gnu-tar.tar
