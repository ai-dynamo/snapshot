#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Create a single-node k3d cluster that Snapshot's CPU e2e suite can run
# against, on any linux/amd64 Docker host (including a GitHub-hosted runner).
#
# This only prepares the cluster. Installing Snapshot stays in the shared
# installer (e2e/snapshot_e2e/infra/setup.py) so the CPU and GPU paths agree on
# what "installed" means -- including the checkpoint PVC, which the installer
# creates. This script supplies only the PersistentVolume behind it, because a
# hostPath volume is the part that is specific to k3d.
#
# There is no GPU here, so the cluster has to satisfy three expectations the
# chart and the test helpers inherited from the GPU environment: the GPU node
# label, a RuntimeClass named nvidia, and a ReadWriteMany checkpoint volume.
# Each is faked below, and each fake is safe only because the cluster has
# exactly one node.

set -euo pipefail

CLUSTER_NAME="${SNAPSHOT_E2E_K3D_CLUSTER:-snapshot-k3d-cpu}"

# A registry the cluster pulls from, so images reach the node once instead of
# twice. The alternative -- `docker buildx --load` followed by `k3d image
# import` -- unpacks every image into the Docker daemon and then re-packs and
# copies it into the node, which for a multi-gigabyte CUDA-based agent image
# costs more than building it.
#
# Addressed as localhost:PORT from the host and mirrored to the registry's
# container name inside the cluster (see registry_mirror_config). localhost is
# the one name both Docker and BuildKit treat as plain-HTTP without extra
# configuration, which is why the same reference is not used on both sides.
REGISTRY_NAME="${SNAPSHOT_E2E_K3D_REGISTRY:-snapshot-registry}"
REGISTRY_PORT="${SNAPSHOT_E2E_K3D_REGISTRY_PORT:-5000}"
# k3d prefixes the container it creates, and that name is what resolves on the
# cluster's Docker network.
REGISTRY_HOST="k3d-${REGISTRY_NAME}"
# Must match what the installer requests, or the claim will not bind to the
# volume created here. Both sides read the same variables.
PVC_SIZE="${SNAPSHOT_E2E_PVC_SIZE:-2Gi}"
STORAGE_CLASS="${SNAPSHOT_E2E_STORAGE_CLASS:-snapshot-e2e-hostpath}"

# The CRDs validate metadata with CEL expressions from the `format` library,
# which the apiserver only exposes from 1.32 on; k3d's default k3s is older and
# rejects them. Track the Kubernetes version the GPU e2e already targets.
K3S_IMAGE="${SNAPSHOT_E2E_K3S_IMAGE:-docker.io/rancher/k3s:v1.32.13-k3s1}"

# Checkpoints live on a host directory bind-mounted into the node, so they
# survive a node restart and can be inspected after a failed run.
HOST_CHECKPOINTS="${SNAPSHOT_E2E_CHECKPOINT_HOST:-${RUNNER_TEMP:-/tmp}/snapshot-checkpoints}"
NODE_CHECKPOINTS="${SNAPSHOT_E2E_CHECKPOINT_NODE:-/checkpoints-data}"

# Deliberately not falling back to KUBECONFIG: this file is overwritten with the
# new cluster's credentials, and a developer whose KUBECONFIG points at a real
# cluster would lose it.
KUBECONFIG_OUT="${SNAPSHOT_E2E_KUBECONFIG:-}"

usage() {
  cat >&2 <<'EOF'
usage: hack/k3d-cpu-e2e.sh <command>

commands:
  registry-up      create the image registry the cluster pulls from. Run this
                   before building images, and before cluster-up, which wires
                   the cluster to whatever registry exists
  registry-prefix  print the registry prefix to tag and push images with
  cluster-up       create the cluster and write a kubeconfig
  cluster-down     delete the cluster and the registry
  cluster-down-keep-registry
                   delete the cluster but leave the registry, so a later run
                   with SNAPSHOT_E2E_SKIP_BUILD can still pull its images
  helm-set         print the chart values this cluster needs, one KEY=VALUE per
                   line, for the installer's SNAPSHOT_E2E_HELM_SET

environment:
  SNAPSHOT_E2E_KUBECONFIG   where to write the kubeconfig (required; the file
                            is overwritten, so do not point it at a kubeconfig
                            you want to keep)
  SNAPSHOT_E2E_K3D_CLUSTER  cluster name (default: snapshot-k3d-cpu)
  SNAPSHOT_E2E_K3S_IMAGE    k3s node image (default: v1.32.13-k3s1)
  SNAPSHOT_E2E_K3D_REGISTRY       registry name (default: snapshot-registry)
  SNAPSHOT_E2E_K3D_REGISTRY_PORT  host port for it (default: 5000)
  SNAPSHOT_E2E_PVC_SIZE, SNAPSHOT_E2E_STORAGE_CLASS,
  SNAPSHOT_E2E_CHECKPOINT_HOST
EOF
  exit 2
}

log() {
  echo "[$(date +%H:%M:%S)] $*"
}

# Named explicitly by each caller rather than fixed, because the registry is
# usable before kubectl is installed and should not insist on it.
require_tools() {
  local missing=()
  local tool
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  if ((${#missing[@]})); then
    echo "missing required tools: ${missing[*]}" >&2
    exit 1
  fi
}

# Idempotent, and deliberately not destructive: the images already in a
# registry are the whole point of SNAPSHOT_E2E_SKIP_BUILD, and recreating it
# empty would leave that run with nothing to pull. Tags are overwritten by the
# next push, so a surviving registry is a cache rather than stale state.
registry_up() {
  require_tools docker k3d
  if registry_exists; then
    # Reuse means reusing its port too. k3d names the container after the
    # registry, so a changed SNAPSHOT_E2E_K3D_REGISTRY_PORT still matches by
    # name, and the push would then go to a port nothing is listening on.
    local bound
    bound="$(registry_bound_port)"
    if [[ -n "${bound}" && "${bound}" != "${REGISTRY_PORT}" ]]; then
      echo "registry ${REGISTRY_HOST} already exists on port ${bound}, not ${REGISTRY_PORT}." >&2
      echo "Delete it with 'k3d registry delete ${REGISTRY_HOST}', or unset SNAPSHOT_E2E_K3D_REGISTRY_PORT." >&2
      exit 1
    fi
    log "reusing registry ${REGISTRY_HOST} on localhost:${REGISTRY_PORT}"
    return
  fi

  log "creating registry ${REGISTRY_HOST} on localhost:${REGISTRY_PORT}"
  # Bound to loopback, not every interface. This registry speaks plain HTTP
  # and accepts pushes from anyone who can reach it, and on a developer
  # machine every interface includes the office network. The cluster is
  # unaffected: the node resolves the registry by container name on the Docker
  # network, never through this published port.
  k3d registry create "${REGISTRY_NAME}" --port "127.0.0.1:${REGISTRY_PORT}"
}

registry_exists() {
  k3d registry list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "${REGISTRY_HOST}"
}

# The host port the existing registry publishes for container port 5000, or
# empty if it publishes none. Read from Docker rather than `k3d registry list`,
# whose column layout is not a stable interface.
registry_bound_port() {
  docker inspect "${REGISTRY_HOST}" \
    --format '{{(index (index .NetworkSettings.Ports "5000/tcp") 0).HostPort}}' \
    2>/dev/null || true
}

registry_down() {
  log "deleting registry ${REGISTRY_HOST}"
  k3d registry delete "${REGISTRY_HOST}" || true
}

# Teach the node that localhost:PORT means the registry container.
#
# The push side and the pull side cannot use one name. From the runner the
# registry is a published port on localhost; from inside the cluster localhost
# is the node itself, and the registry is another container on the same Docker
# network. Writing the mirror here lets both sides keep the reference they can
# actually resolve, while the image is pushed and pulled under one tag.
registry_mirror_config() {
  cat <<EOF
mirrors:
  "localhost:${REGISTRY_PORT}":
    endpoint:
      - http://${REGISTRY_HOST}:5000
EOF
}

create_cluster() {
  if k3d cluster list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "${CLUSTER_NAME}"; then
    log "deleting leftover cluster ${CLUSTER_NAME}"
    k3d cluster delete "${CLUSTER_NAME}"
  fi

  # Expanded below as ${registry_args[@]+...}, because bash 3.2 -- still what
  # macOS ships -- treats an empty "${array[@]}" under `set -u` as unbound.
  local registry_args=()
  if registry_exists; then
    local config
    config="$(mktemp)"
    registry_mirror_config >"${config}"
    # --registry-use joins the registry to the cluster network so the node can
    # resolve it at all; --registry-config is what makes the localhost tag
    # resolve there. Both are needed, and neither implies the other.
    registry_args=(
      --registry-use "${REGISTRY_HOST}:${REGISTRY_PORT}"
      --registry-config "${config}"
    )
    log "wiring cluster to registry ${REGISTRY_HOST}"
  else
    log "no registry ${REGISTRY_HOST}; the cluster will only see preloaded images"
  fi

  log "creating cluster ${CLUSTER_NAME} (${K3S_IMAGE})"
  # k3d nodes are privileged by default, which CRIU needs in order to freeze and
  # dump a workload. Traefik is dead weight here.
  k3d cluster create "${CLUSTER_NAME}" \
    --wait \
    --image "${K3S_IMAGE}" \
    --kubeconfig-update-default=false \
    --kubeconfig-switch-context=false \
    --k3s-arg '--disable=traefik@server:0' \
    ${registry_args[@]+"${registry_args[@]}"} \
    --volume "${HOST_CHECKPOINTS}:${NODE_CHECKPOINTS}@server:0"

  mkdir -p "$(dirname "${KUBECONFIG_OUT}")"
  k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_OUT}"
  chmod 0600 "${KUBECONFIG_OUT}"
}

fake_gpu_nodes() {
  # The agent DaemonSet and the test workloads both select GPU nodes. Labelling
  # the only node we have is what lets them schedule without a GPU.
  log "labelling nodes as GPU-present"
  local node
  for node in $(kubectl get nodes -o name); do
    kubectl label "${node}" --overwrite nvidia.com/gpu.present=true
  done
}

fake_nvidia_runtime_class() {
  # The agent pod defaults to runtimeClassName: nvidia. k3s ships its own nvidia
  # RuntimeClass and `handler` is immutable, so this has to be replaced rather
  # than applied over.
  log "pointing RuntimeClass/nvidia at the runc handler"
  kubectl delete runtimeclass nvidia --ignore-not-found
  kubectl create -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: runc
EOF
}

fake_rwx_checkpoint_volume() {
  # The installer requires the checkpoint PVC to offer ReadWriteMany. Kubernetes
  # does not verify that the backing store really is shared, so a hostPath PV
  # declared RWX is accepted. That is only honest on a single-node cluster,
  # where every pod mounting it lands on the same machine.
  #
  # No provisioner serves this storage class, so the claim the installer creates
  # binds to this volume instead of being dynamically provisioned.
  log "creating RWX checkpoint volume (${NODE_CHECKPOINTS})"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: snapshot-e2e-checkpoints
spec:
  capacity:
    storage: ${PVC_SIZE}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${STORAGE_CLASS}
  hostPath:
    path: ${NODE_CHECKPOINTS}
    type: DirectoryOrCreate
EOF

  kubectl wait --for=jsonpath='{.status.phase}'=Available \
    pv/snapshot-e2e-checkpoints --timeout=60s
}

# k3s keeps containerd somewhere other than the conventional paths, and the
# single node also has to fit the workload pods. These are properties of the
# cluster, not of the caller, so they live here rather than in CI.
#
# The chart sizes the agent and PageBroker for a GPU node. CPU-only tests
# allocate no CUDA staging ring and must leave room for workloads on the runner.
# If a component is OOMKilled mid-dump, its memory limit is the first thing to raise.
print_helm_set() {
  cat <<EOF
runtime.socketPath=/run/k3s/containerd/containerd.sock
runtime.storageDir=/var/lib/rancher/k3s/agent/containerd
daemonset.resources.requests.cpu=200m
daemonset.resources.requests.memory=256Mi
daemonset.resources.limits.cpu=2
daemonset.resources.limits.memory=2Gi
pageBroker.resources.requests.cpu=200m
pageBroker.resources.requests.memory=256Mi
pageBroker.resources.limits.cpu=2
pageBroker.resources.limits.memory=2Gi
EOF
}

cluster_up() {
  require_tools docker k3d kubectl
  if [[ -z "${KUBECONFIG_OUT}" ]]; then
    echo "SNAPSHOT_E2E_KUBECONFIG is required: it names the file to write" >&2
    exit 1
  fi

  # The directory is created by whoever runs this script, as that user, with
  # mode 0755, and is then the checkpoint PVC inside the node. PageBroker
  # writes as a different UID and previously failed its first Commit here
  # with "cannot create directories: Permission denied" under
  # /checkpoints/artifacts. Make it world-writable, as a dynamic hostPath
  # provisioner does for the volumes it hands out.
  mkdir -p "${HOST_CHECKPOINTS}"
  chmod 1777 "${HOST_CHECKPOINTS}"
  create_cluster
  export KUBECONFIG="${KUBECONFIG_OUT}"

  fake_gpu_nodes
  fake_nvidia_runtime_class
  fake_rwx_checkpoint_volume

  kubectl get nodes -o wide
  log "cluster ${CLUSTER_NAME} ready (KUBECONFIG=${KUBECONFIG_OUT})"
}

cluster_down() {
  log "deleting cluster ${CLUSTER_NAME}"
  k3d cluster delete "${CLUSTER_NAME}" || true
  # After the cluster: deleting the registry first would leave the node with a
  # mirror pointing at nothing, and k3d logs that as an error on teardown.
  if [[ "${1:-}" == "keep-registry" ]]; then
    log "leaving registry ${REGISTRY_HOST} up"
    return
  fi
  registry_down
}

case "${1:-}" in
registry-up) registry_up ;;
registry-prefix) echo "localhost:${REGISTRY_PORT}" ;;
cluster-up) cluster_up ;;
cluster-down) cluster_down ;;
cluster-down-keep-registry) cluster_down keep-registry ;;
helm-set) print_helm_set ;;
*) usage ;;
esac
