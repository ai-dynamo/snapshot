#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Run from the repository root with the build-image environment supplied by CI.
# Separate make processes let image builds overlap despite .NOTPARALLEL.
set -euo pipefail

pids=()
logs=()
declare -A workers=()
started=$(date +%s)

cleanup() {
  local status=$? pid logfile
  trap - EXIT INT TERM
  local -A groups=()
  local stopping=()
  # Include the last launch if a signal arrived before registration, and
  # children whose worker leader has already exited.
  for pid in "${pids[@]}" $(jobs -pr); do
    groups["${pid}"]=1
  done
  for pid in "${!groups[@]}"; do
    if kill -0 "${pid}" 2>/dev/null || kill -0 -- "-${pid}" 2>/dev/null; then
      stopping+=("${pid}")
      # Cancellation can arrive before setsid creates the process group.
      kill "${pid}" 2>/dev/null || true
      kill -- "-${pid}" 2>/dev/null || true
    fi
  done
  if ((${#stopping[@]})); then
    sleep 1
    for pid in "${stopping[@]}"; do
      kill -KILL "${pid}" 2>/dev/null || true
      kill -KILL -- "-${pid}" 2>/dev/null || true
    done
  fi
  wait || true
  # Flush after termination so cancellation retains each worker's final output.
  for logfile in "${logs[@]}"; do
    [[ -f "${logfile}" ]] || continue
    echo "::group::${logfile##*/}"
    cat "${logfile}" || true
    echo "::endgroup::"
  done
  if ! bash hack/ci-phase.sh "Build images and create k3d cluster" "${started}"; then
    if [ "${status}" -eq 0 ]; then
      status=1
    fi
  fi
  exit "${status}"
}

start_worker() {
  local name="$1"
  shift
  local logfile="${RUNNER_TEMP}/cpu-${name}.log"
  logs+=("${logfile}")
  setsid "$@" >"${logfile}" 2>&1 &
  local pid=$!
  pids+=("${pid}")
  workers["${pid}"]="${name}"
}

start_image_build() {
  local image="$1" args="" phase
  phase="Build ${image} image"
  if [ "${image}" = operator ]; then
    args="--build-arg GO_BUILD_TAGS=snapshot_e2e"
  fi
  if [ "${image}" = pagebroker ]; then
    phase="Build PageBroker image"
  fi
  start_worker "build-${image}" env \
    IMAGE="${image}" BUILD_ARGS="${args}" \
    PHASE="${phase}" CI_PHASE_LOG="${CI_PHASE_LOG}.builds" \
    GITHUB_OUTPUT="${RUNNER_TEMP}/cpu-build-${image}.outputs" \
    bash hack/ci-build-image.sh
}

finish_worker() {
  local pid="$1" status="$2"
  if [ "${status}" -ne 0 ]; then
    echo "::error::CPU worker ${workers[${pid}]} exited with status ${status}" >&2
    return "${status}"
  fi
  unset "workers[${pid}]"
}

wait_for_workers() {
  local pid status completed
  while ((${#workers[@]})); do
    # wait -n cannot collect a job that finished before the call began.
    for pid in "${!workers[@]}"; do
      if ! kill -0 "${pid}" 2>/dev/null; then
        if wait "${pid}"; then
          status=0
        else
          status=$?
        fi
        finish_worker "${pid}" "${status}" || return "$?"
      fi
    done
    ((${#workers[@]})) || break
    if wait -n -p completed "${!workers[@]}" 2>/dev/null; then
      status=0
    else
      status=$?
    fi
    # A job can also finish between the scan and wait -n. Re-scan if Bash
    # reports no pending child rather than a completed PID.
    if [[ -n "${completed:-}" ]]; then
      finish_worker "${completed}" "${status}" || return "$?"
    fi
  done
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for image in operator agent pagebroker; do
  start_image_build "${image}"
done
start_worker cluster-up bash hack/k3d-cpu-e2e.sh cluster-up
wait_for_workers
