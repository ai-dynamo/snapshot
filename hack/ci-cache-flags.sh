#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Prints the buildx cache flags for one image, on one branch.
#
# The cache is keyed by branch. main and release/* both publish artifacts, and
# an unkeyed tag meant a release push overwrote the cache every pull request
# against main reads, sending the next agent build cold for no reason the
# author could see.
#
# Usage: ci-cache-flags.sh <read|write> <image> <branch>
#
#   read   one --cache-from for the branch, one for main behind it so a pull
#          request against a release branch still starts from main's layers
#          when its own tag is missing or new, and one for the pre-keying tag
#          behind both. Pull requests never write: a cache a pull request can
#          write is a cache a fork can poison.
#   write  --cache-to for the branch, and --cache-from for the branch with main
#          behind it, as in read mode. push-artifacts.yaml reaches this for
#          pushes to main and release/*, and for a manual run on any other
#          branch; without the fallback, a branch's first build found no cache
#          of its own and built every layer from scratch. A release runs the
#          same workflow in read mode, because a tag never recurs and so a
#          cache published under one is a cache nothing ever reads.
#
# Reads CACHE_REF_PREFIX, and CACHE_TAG_SUFFIX if set — any unused value there
# points every flag at a tag that does not exist, which is how a cold build is
# measured without disturbing the cache other runs are reading.

set -eu

MODE=${1:?usage: ci-cache-flags.sh <read|write> <image> <branch>}
IMAGE=${2:?usage: ci-cache-flags.sh <read|write> <image> <branch>}
BRANCH=${3:?usage: ci-cache-flags.sh <read|write> <image> <branch>}

: "${CACHE_REF_PREFIX:?CACHE_REF_PREFIX must name the cache repository}"
SUFFIX=${CACHE_TAG_SUFFIX:-}

# Validated here rather than trusted, because it arrives from a
# workflow_dispatch input and lands in a string the Makefile word-splits as
# $(DOCKER_BUILD_ARGS). A space or a quote would not merely name a tag that
# cannot exist -- it would add build flags.
case "$SUFFIX" in
*[!A-Za-z0-9._-]*)
    echo "CACHE_TAG_SUFFIX may only contain letters, digits, '.', '_' or '-': '$SUFFIX'" >&2
    exit 2
    ;;
esac

# Same shape as the image tag slug in push-artifacts.yaml: lowercase
# alphanumerics and single hyphens. A tag cannot hold the slash in release/0.1.
slug() {
    printf '%s' "$1" |
        tr '[:upper:]' '[:lower:]' |
        sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

# Portable: sha256sum on the runners, shasum on a developer's Mac.
short_digest() {
    if command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$1" | sha256sum | cut -c1-7
    else
        printf '%s' "$1" | shasum -a 256 | cut -c1-7
    fi
}

branch_slug=$(slug "$BRANCH")
branch_slug=${branch_slug:-branch}

# The slug is lossy -- release/a_b and release/a-b both reduce to release-a-b --
# so every branch but main carries a digest of its real name, and two branches
# cannot land on one cache. main is exempt because its tag is the one humans
# read, and nothing else can collide with a name that short.
#
# Truncated as well as suffixed: a tag is capped at 128 characters and nothing
# caps a branch name, and an over-length reference fails the build outright
# rather than merely missing the cache.
if [ "$branch_slug" != main ]; then
    if [ "${#branch_slug}" -gt 24 ]; then
        branch_slug=$(printf '%s' "$branch_slug" | cut -c1-24)
    fi
    branch_slug="${branch_slug}-$(short_digest "$BRANCH")"
fi

ref="${CACHE_REF_PREFIX}:${IMAGE}-${branch_slug}${SUFFIX}"
main_ref="${CACHE_REF_PREFIX}:${IMAGE}-main${SUFFIX}"

# The tag the cache used before it was keyed by branch. Nothing writes it any
# more, so it only ages out; it is read so the branch-keyed tags being empty
# until main first publishes them does not send every build cold in the
# meantime. Remove once a push to main has written the keyed tags.
legacy_ref="${CACHE_REF_PREFIX}:${IMAGE}${SUFFIX}"

case "$MODE" in
read)
    printf '%s' "--cache-from=type=registry,ref=${ref}"
    # Nothing to fall back to when the branch is main, and importing the same
    # cache twice is not free.
    if [ "$ref" != "$main_ref" ]; then
        printf ' %s' "--cache-from=type=registry,ref=${main_ref}"
    fi
    printf ' %s' "--cache-from=type=registry,ref=${legacy_ref}"
    ;;
write)
    printf '%s' "--cache-from=type=registry,ref=${ref}"
    if [ "$ref" != "$main_ref" ]; then
        printf ' %s' "--cache-from=type=registry,ref=${main_ref}"
    fi
    # image-manifest and oci-mediatypes because GHCR rejects buildkit's
    # default cache manifest type.
    printf ' %s' "--cache-to=type=registry,ref=${ref},mode=max,image-manifest=true,oci-mediatypes=true"
    ;;
*)
    echo "unknown mode: $MODE (expected read or write)" >&2
    exit 2
    ;;
esac
