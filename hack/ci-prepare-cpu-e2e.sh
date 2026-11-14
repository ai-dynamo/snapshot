#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Run from the repository root with the build-image environment supplied by CI.
# Separate make processes let image builds overlap despite .NOTPARALLEL.
set -euo pipefail

pids=()
logs=()

cleanup() {
  trap - EXIT INT TERM
  # Include descendants such as make/buildx. The live job table also catches
  # a worker started just before its PID was added to the wait list.
  local pid
  for pid in $(jobs -pr); do
    kill -- "-${pid}" 2>/dev/null || true
  done
  wait || true
}

start_worker() {
  local name="$1"
  shift
  local logfile="${RUNNER_TEMP}/cpu-${name}.log"
  logs+=("${logfile}")
  setsid "$@" >"${logfile}" 2>&1 &
  pids+=("$!")
}

start_image_build() {
  local image="$1" variant="" args=""
  if [ "${image}" = operator ]; then
    variant=snapshot-e2e
    args="--build-arg GO_BUILD_TAGS=snapshot_e2e"
  fi
  start_worker "build-${image}" env \
    IMAGE="${image}" BUILD_ARGS="${args}" CACHE_VARIANT="${variant}" \
    PHASE="Build ${image} image" \
    GITHUB_OUTPUT="${RUNNER_TEMP}/cpu-build-${image}.outputs" \
    bash hack/ci-build-image.sh
}

wait_for_workers() {
  local status=0 pid logfile
  for pid in "${pids[@]}"; do
    wait "${pid}" || status=1
  done
  for logfile in "${logs[@]}"; do
    echo "::group::${logfile##*/}"
    cat "${logfile}"
    echo "::endgroup::"
  done
  return "${status}"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for image in operator agent pagebroker; do
  start_image_build "${image}"
done
start_worker cluster-up bash hack/k3d-cpu-e2e.sh cluster-up
wait_for_workers
