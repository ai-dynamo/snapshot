#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the CPU e2e suite end to end on a local Docker host: build the images,
# create a k3d cluster, install Snapshot into it, and run the tests.
#
# This is the local counterpart of .github/workflows/e2e-cpu.yaml and runs the
# same sequence against the same two building blocks -- hack/k3d-cpu-e2e.sh for
# the cluster and snapshot_e2e.infra.setup for the install. CI spells the steps
# out separately so it can time them and collect diagnostics on failure; keep
# the two in step when either changes.
#
# Each stage is also usable on its own. Against a cluster that already has
# Snapshot installed, skip this script and run pytest directly; against a clean
# cluster, run the installer directly. See e2e/README.md.

set -euo pipefail

cd "$(dirname "$0")/.."

: "${IMAGE_REGISTRY:=snapshot.local}"
: "${IMAGE_TAG:=cpu-e2e}"

# Shared with hack/k3d-cpu-e2e.sh and the installer, which read the same names.
export SNAPSHOT_E2E_MODE="${SNAPSHOT_E2E_MODE:-direct}"
export SNAPSHOT_E2E_K3D_CLUSTER="${SNAPSHOT_E2E_K3D_CLUSTER:-snapshot-k3d-cpu}"
export SNAPSHOT_E2E_STORAGE_CLASS="${SNAPSHOT_E2E_STORAGE_CLASS:-snapshot-e2e-hostpath}"
export SNAPSHOT_E2E_SNAPSHOT_TAG="${SNAPSHOT_E2E_SNAPSHOT_TAG:-${IMAGE_TAG}}"
export SNAPSHOT_E2E_WORKLOAD_IMAGE="${SNAPSHOT_E2E_WORKLOAD_IMAGE:-${IMAGE_REGISTRY}/agent:${IMAGE_TAG}}"
export SNAPSHOT_E2E_KUBECONFIG="${SNAPSHOT_E2E_KUBECONFIG:-${PWD}/.local/kubeconfig-cpu-e2e}"
export KUBECONFIG="${SNAPSHOT_E2E_KUBECONFIG}"

SKIP_BUILD="${SNAPSHOT_E2E_SKIP_BUILD:-false}"
KEEP_CLUSTER="${SNAPSHOT_E2E_KEEP_CLUSTER:-false}"
PYTEST_ARGS=("$@")
if ((${#PYTEST_ARGS[@]} == 0)); then
  PYTEST_ARGS=(-m cpu -vv)
fi

log() {
  echo "[$(date +%H:%M:%S)] $*"
}

require_linux_amd64_docker() {
  local os arch
  os="$(docker version --format '{{.Server.Os}}')"
  arch="$(docker version --format '{{.Server.Arch}}')"
  # The agent image ships linux/amd64 only, because cuda-checkpoint does. The
  # k3d node runs on the same daemon, so the daemon has to match.
  if [[ "${os}/${arch}" != "linux/amd64" ]]; then
    echo "the CPU e2e suite needs a linux/amd64 Docker daemon, found ${os}/${arch}" >&2
    exit 1
  fi
}

cleanup() {
  if [[ "${KEEP_CLUSTER}" == "true" ]]; then
    log "leaving cluster ${SNAPSHOT_E2E_K3D_CLUSTER} up (KUBECONFIG=${KUBECONFIG})"
    return
  fi
  hack/k3d-cpu-e2e.sh cluster-down
}

require_linux_amd64_docker

if [[ "${SKIP_BUILD}" != "true" ]]; then
  log "building images"
  make docker-build-operator REGISTRY="${IMAGE_REGISTRY}" TAGS="${IMAGE_TAG}" \
    DOCKER_BUILD_ARGS="--load"
  make docker-build-agent REGISTRY="${IMAGE_REGISTRY}" TAGS="${IMAGE_TAG}" \
    DOCKER_BUILD_ARGS="--load"
fi

# Registered before the cluster exists: cluster-up can create the node and then
# fail while configuring it, and cluster-down tolerates there being nothing to
# delete.
trap cleanup EXIT
hack/k3d-cpu-e2e.sh cluster-up

log "importing images into ${SNAPSHOT_E2E_K3D_CLUSTER}"
k3d image import \
  "${IMAGE_REGISTRY}/operator:${IMAGE_TAG}" \
  "${IMAGE_REGISTRY}/agent:${IMAGE_TAG}" \
  --cluster "${SNAPSHOT_E2E_K3D_CLUSTER}"

log "installing Snapshot"
# The helper is alone in its substitution, and assigned rather than exported, so
# that `set -e` sees it fail. A substitution reports the status of its *last*
# command, so appending the image lines inside it would hide a failure here, and
# `export` would hide it again.
cluster_values="$(hack/k3d-cpu-e2e.sh helm-set)"
export SNAPSHOT_E2E_HELM_SET="${cluster_values}
image.operator.repository=${IMAGE_REGISTRY}/operator
image.agent.repository=${IMAGE_REGISTRY}/agent
image.operator.pullPolicy=IfNotPresent
image.agent.pullPolicy=IfNotPresent"
uv run --locked --project e2e python -m snapshot_e2e.infra.setup \
  --phase snapshot-install --skip-host-preflight
uv run --locked --project e2e python -m snapshot_e2e.infra.setup \
  --phase snapshot-ready

log "running tests"
uv run --locked --project e2e pytest e2e/tests "${PYTEST_ARGS[@]}"
