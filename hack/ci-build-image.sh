#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Shared by the build-image action and the concurrent CPU image builds.
# The action supplies these environment variables and their defaults. CPU
# builds supply the same values, with a separate output file per image.

set -euo pipefail
start=$(date +%s)
case "${PUSH}" in
  true) push_flag=--push ;;
  # Untagged, so no tag ever points at a single architecture; whoever
  # joins the architectures into an index tags that instead.
  digest) push_flag="--output type=image,name=${REGISTRY}/${IMAGE},push-by-digest=true,name-canonical=true,push=true" ;;
  false) push_flag= ;;
  *) echo "::error::push must be \"true\", \"digest\", or \"false\", not \"${PUSH}\"" >&2; exit 2 ;;
esac
# Separate variants keep their own final layers, while the standard cache
# remains a fallback for shared stages and for the first variant build.
variant="${CACHE_VARIANT:-}"
if [[ ! "${variant}" =~ ^[A-Za-z0-9._-]*$ ]]; then
  echo "::error::cache variant must contain only letters, digits, '.', '_' or '-'" >&2
  exit 2
fi
cache_key="${IMAGE}${variant:+-${variant}}"
base_key="${IMAGE}"
if [ "${PLATFORM}" != linux/amd64 ]; then
  cache_key="${cache_key}-${PLATFORM#linux/}"
  base_key="${base_key}-${PLATFORM#linux/}"
fi
# Assigned, not inlined into the argument: a command substitution that
# fails inside a simple command does not trip `set -e`, so an inline
# call would drop the cache flags and still build -- silently cold, or
# silently publishing without --cache-to.
cache_flags="$(hack/ci-cache-flags.sh "${CACHE_MODE}" "${cache_key}" "${CACHE_BRANCH}")"
if [ -n "${variant}" ]; then
  base_flags="$(hack/ci-cache-flags.sh read "${base_key}" "${CACHE_BRANCH}")"
  cache_flags="${cache_flags} ${base_flags}"
fi
metadata="$(mktemp "${RUNNER_TEMP}/build-image-${IMAGE}.XXXXXX.json")"
trap 'rm -f "${metadata}"' EXIT
make_args=("REGISTRY=${REGISTRY}" "AGENT_PLATFORM=${PLATFORM}")
if [ "${PUSH}" = digest ]; then
  make_args+=("TAGS=")
elif [ -n "${TAGS}" ]; then
  make_args+=("TAGS=${TAGS}")
fi
make "docker-build-${IMAGE}" "${make_args[@]}" \
  DOCKER_BUILD_ARGS="${push_flag} ${BUILD_ARGS} --metadata-file=${metadata} ${cache_flags}"
digest="$(jq -r '."containerimage.digest" // empty' "${metadata}")"
# A pushed image is consumed by digest -- signed, or installed -- so a
# push that reports none is a failure, not an empty output.
if [ "${PUSH}" != false ] && [ -z "${digest}" ]; then
  echo "::error::buildx reported no digest for the pushed ${IMAGE} image" >&2
  exit 1
fi
echo "digest=${digest}" >> "${GITHUB_OUTPUT}"
if [ -n "${PHASE}" ]; then
  hack/ci-phase.sh "${PHASE}" "${start}" "${REGISTRY}/${IMAGE}:${TAGS%% *}"
fi
