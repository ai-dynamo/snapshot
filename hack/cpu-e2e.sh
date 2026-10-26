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

# The k3d-managed registry the cluster pulls from, created below. Images are
# pushed to it rather than loaded into Docker and copied into the node, which
# moved every image twice.
IMAGE_REGISTRY="${IMAGE_REGISTRY:-$(hack/k3d-cpu-e2e.sh registry-prefix)}"
: "${IMAGE_TAG:=cpu-e2e}"

# Shared with hack/k3d-cpu-e2e.sh and the installer, which read the same names.
export SNAPSHOT_E2E_MODE="${SNAPSHOT_E2E_MODE:-direct}"
export SNAPSHOT_E2E_CPU_ONLY=true
export SNAPSHOT_E2E_K3D_CLUSTER="${SNAPSHOT_E2E_K3D_CLUSTER:-snapshot-k3d-cpu}"
export SNAPSHOT_E2E_STORAGE_CLASS="${SNAPSHOT_E2E_STORAGE_CLASS:-snapshot-e2e-hostpath}"
export SNAPSHOT_E2E_SNAPSHOT_TAG="${SNAPSHOT_E2E_SNAPSHOT_TAG:-${IMAGE_TAG}}"
export SNAPSHOT_E2E_WORKLOAD_IMAGE="${SNAPSHOT_E2E_WORKLOAD_IMAGE:-${IMAGE_REGISTRY}/agent:${IMAGE_TAG}}"
export SNAPSHOT_E2E_KUBECONFIG="${SNAPSHOT_E2E_KUBECONFIG:-${PWD}/.local/kubeconfig-cpu-e2e}"
export KUBECONFIG="${SNAPSHOT_E2E_KUBECONFIG}"

SKIP_BUILD="${SNAPSHOT_E2E_SKIP_BUILD:-false}"
KEEP_CLUSTER="${SNAPSHOT_E2E_KEEP_CLUSTER:-false}"
STORAGE="${SNAPSHOT_E2E_STORAGE:-pvc}"
case "${STORAGE}" in
  pvc | s3) ;;
  *)
    echo "SNAPSHOT_E2E_STORAGE must be pvc or s3, got ${STORAGE}" >&2
    exit 1
    ;;
esac
PYTEST_ARGS=("$@")
if ((${#PYTEST_ARGS[@]} == 0)); then
  if [[ "${STORAGE}" == "s3" ]]; then
    PYTEST_ARGS=(-m s3 -vv)
  else
    PYTEST_ARGS=(-m cpu -vv)
  fi
fi

log() {
  echo "[$(date +%H:%M:%S)] $*"
}

require_linux_amd64_docker() {
  local os arch
  os="$(docker version --format '{{.Server.Os}}')"
  arch="$(docker version --format '{{.Server.Arch}}')"
  # The suite builds the agent for linux/amd64 by default, and the k3d node runs
  # on the same daemon, so the daemon has to match.
  if [[ "${os}/${arch}" != "linux/amd64" ]]; then
    echo "the CPU e2e suite needs a linux/amd64 Docker daemon, found ${os}/${arch}" >&2
    exit 1
  fi
}

require_push_capable_builder() {
  # The default `docker` driver can load into the daemon but cannot push, and
  # pushing is how images reach the cluster now. A container-driver builder
  # can do both; create one once and reuse it.
  local builder=snapshot-cpu-e2e
  if ! docker buildx inspect "${builder}" >/dev/null 2>&1; then
    log "creating buildx builder ${builder}"
    # Host networking so localhost means the runner, and therefore the
    # registry's published port, rather than the builder's own namespace.
    docker buildx create --name "${builder}" --driver docker-container \
      --driver-opt network=host >/dev/null
  fi
  # Selected for this process, not with `buildx use`, which rewrites the
  # machine's default builder and outlives the script -- leaving every
  # unrelated `docker build` on the container driver afterwards.
  export BUILDX_BUILDER="${builder}"
}

cleanup() {
  if [[ "${KEEP_CLUSTER}" == "true" ]]; then
    log "leaving cluster ${SNAPSHOT_E2E_K3D_CLUSTER} up (KUBECONFIG=${KUBECONFIG})"
    return
  fi
  if [[ "${SKIP_BUILD}" == "true" ]]; then
    # The images came from an earlier run, so the registry is not this run's to
    # delete: doing so would destroy what the next skip-build run needs and
    # leave it failing the preflight below.
    log "leaving registry up: this run borrowed its images"
    hack/k3d-cpu-e2e.sh cluster-down-keep-registry
    return
  fi
  hack/k3d-cpu-e2e.sh cluster-down
}

require_linux_amd64_docker

# Registered before anything is created: the registry comes up first and
# cluster-down removes both, and each tolerates there being nothing to delete.
trap cleanup EXIT

# Before the builds, which push into it, and before the cluster, which is
# wired to whatever registry exists at the time it is created.
hack/k3d-cpu-e2e.sh registry-up

if [[ "${SKIP_BUILD}" != "true" ]]; then
  require_push_capable_builder
  log "building images"
  for component in operator agent pagebroker; do
    make "docker-build-${component}" REGISTRY="${IMAGE_REGISTRY}" TAGS="${IMAGE_TAG}" \
      DOCKER_BUILD_ARGS="--push"
  done
else
  # Skipping the builds means relying on what a previous run pushed, and the
  # registry only survives that run if it kept the cluster. Say so here rather
  # than let the pods fail to pull ten minutes later.
  missing=()
  for component in operator agent pagebroker; do
    curl -fsS "http://${IMAGE_REGISTRY}/v2/${component}/manifests/${IMAGE_TAG}" \
      -H 'Accept: application/vnd.oci.image.index.v1+json' \
      -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
      -o /dev/null 2>/dev/null || missing+=("${component}:${IMAGE_TAG}")
  done
  if ((${#missing[@]})); then
    echo "SNAPSHOT_E2E_SKIP_BUILD is set but ${IMAGE_REGISTRY} has no ${missing[*]}." >&2
    echo "Run once without it, with SNAPSHOT_E2E_KEEP_CLUSTER=true so the registry survives." >&2
    exit 1
  fi
  log "reusing images already in ${IMAGE_REGISTRY}"
fi

hack/k3d-cpu-e2e.sh cluster-up

log "installing Snapshot"
# The helper is alone in its substitution, and assigned rather than exported, so
# that `set -e` sees it fail. A substitution reports the status of its *last*
# command, so appending the image lines inside it would hide a failure here, and
# `export` would hide it again.
cluster_values="$(hack/k3d-cpu-e2e.sh helm-set)"
export SNAPSHOT_E2E_HELM_SET="${cluster_values}
image.operator.repository=${IMAGE_REGISTRY}/operator
image.agent.repository=${IMAGE_REGISTRY}/agent
image.pageBroker.repository=${IMAGE_REGISTRY}/pagebroker
image.operator.pullPolicy=IfNotPresent
image.agent.pullPolicy=IfNotPresent
image.pageBroker.pullPolicy=IfNotPresent"
if [[ "${STORAGE}" == "s3" ]]; then
  s3_gateway="$(docker network inspect "k3d-${SNAPSHOT_E2E_K3D_CLUSTER}" \
    --format '{{(index .IPAM.Config 0).Gateway}}')"
  export SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT="${SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT:-http://${s3_gateway}:9000}"
  uv run --locked --project e2e python -m snapshot_e2e.s3 prepare
  s3_values="$(uv run --locked --project e2e python -m snapshot_e2e.s3 helm-set)"
  export SNAPSHOT_E2E_HELM_SET="${SNAPSHOT_E2E_HELM_SET}
${s3_values}"
fi
uv run --locked --project e2e python -m snapshot_e2e.infra.setup \
  --phase snapshot-install --skip-host-preflight
uv run --locked --project e2e python -m snapshot_e2e.infra.setup \
  --phase snapshot-ready

log "running tests"
uv run --locked --project e2e pytest e2e/tests "${PYTEST_ARGS[@]}"
