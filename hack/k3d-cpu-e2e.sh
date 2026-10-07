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
  cluster-up     create the cluster and write a kubeconfig
  cluster-down   delete the cluster
  helm-set       print the chart values this cluster needs, one KEY=VALUE per
                 line, for the installer's SNAPSHOT_E2E_HELM_SET

environment:
  SNAPSHOT_E2E_KUBECONFIG   where to write the kubeconfig (required; the file
                            is overwritten, so do not point it at a kubeconfig
                            you want to keep)
  SNAPSHOT_E2E_K3D_CLUSTER  cluster name (default: snapshot-k3d-cpu)
  SNAPSHOT_E2E_K3S_IMAGE    k3s node image (default: v1.32.13-k3s1)
  SNAPSHOT_E2E_PVC_SIZE, SNAPSHOT_E2E_STORAGE_CLASS,
  SNAPSHOT_E2E_CHECKPOINT_HOST
EOF
  exit 2
}

log() {
  echo "[$(date +%H:%M:%S)] $*"
}

require_tools() {
  local missing=()
  local tool
  for tool in docker k3d kubectl; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  if ((${#missing[@]})); then
    echo "missing required tools: ${missing[*]}" >&2
    exit 1
  fi
}

create_cluster() {
  if k3d cluster list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "${CLUSTER_NAME}"; then
    log "deleting leftover cluster ${CLUSTER_NAME}"
    k3d cluster delete "${CLUSTER_NAME}"
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
# The chart sizes the agent for a GPU node: 2 CPU requested of the 4 a runner
# has, which leaves a test workload nowhere to schedule. The values below are
# sized for that runner rather than measured, so if the agent is OOMKilled
# mid-dump, the memory limit is the first thing to raise.
print_helm_set() {
  cat <<EOF
runtime.socketPath=/run/k3s/containerd/containerd.sock
runtime.storageDir=/var/lib/rancher/k3s/agent/containerd
daemonset.resources.requests.cpu=200m
daemonset.resources.requests.memory=256Mi
daemonset.resources.limits.cpu=2
daemonset.resources.limits.memory=2Gi
EOF
}

cluster_up() {
  require_tools
  if [[ -z "${KUBECONFIG_OUT}" ]]; then
    echo "SNAPSHOT_E2E_KUBECONFIG is required: it names the file to write" >&2
    exit 1
  fi

  mkdir -p "${HOST_CHECKPOINTS}"
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
}

case "${1:-}" in
cluster-up) cluster_up ;;
cluster-down) cluster_down ;;
helm-set) print_helm_set ;;
*) usage ;;
esac
