#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Shared by the build-image action and concurrent CPU builds. Callers supply
# the environment and an output file for each build.

set -euo pipefail
start=$(date +%s)
case "${PUSH}" in
  true) push_flag=--push ;;
  # The release job tags the joined index, keeping each architecture untagged.
  digest) push_flag="--output type=image,name=${REGISTRY}/${IMAGE},push-by-digest=true,name-canonical=true,push=true" ;;
  false) push_flag= ;;
  *) echo "::error::push must be \"true\", \"digest\", or \"false\", not \"${PUSH}\"" >&2; exit 2 ;;
esac
cache_key="${IMAGE}"
if [ "${PLATFORM}" != linux/amd64 ]; then
  cache_key="${IMAGE}-${PLATFORM#linux/}"
fi
# Assign separately so set -e catches cache-script failures before make runs.
cache_flags="$(hack/ci-cache-flags.sh "${CACHE_MODE}" "${cache_key}" "${CACHE_BRANCH}")"
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
# Signing and installation require the digest of every pushed image.
if [ "${PUSH}" != false ] && [ -z "${digest}" ]; then
  echo "::error::buildx reported no digest for the pushed ${IMAGE} image" >&2
  exit 1
fi
echo "digest=${digest}" >> "${GITHUB_OUTPUT}"
if [ -n "${PHASE}" ]; then
  hack/ci-phase.sh "${PHASE}" "${start}" "${REGISTRY}/${IMAGE}:${TAGS%% *}"
fi
