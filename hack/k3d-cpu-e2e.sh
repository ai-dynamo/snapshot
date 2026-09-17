#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Prepare a single-node privileged k3d cluster for CPU Snapshot e2e (issue 241 /
# gap 14). Does not install Snapshot; the workflow Helm-installs after images
# are imported.

set -euo pipefail

CLUSTER_NAME="${SNAPSHOT_E2E_K3D_CLUSTER:-snapshot-k3d-cpu}"
TEST_NAMESPACE="${SNAPSHOT_E2E_TEST_NAMESPACE:-snapshot-e2e}"
PVC_NAME="${SNAPSHOT_E2E_PVC_NAME:-snapshot-pvc}"
PVC_SIZE="${SNAPSHOT_E2E_PVC_SIZE:-2Gi}"
HOST_CHECKPOINTS="${SNAPSHOT_E2E_CHECKPOINT_HOST:-${RUNNER_TEMP:-/tmp}/snapshot-checkpoints}"
NODE_CHECKPOINTS="${SNAPSHOT_E2E_CHECKPOINT_NODE:-/checkpoints-data}"
KUBECONFIG_OUT="${SNAPSHOT_E2E_KUBECONFIG:-${KUBECONFIG:-}}"

usage() {
  echo "usage: $0 cluster-up | cluster-down" >&2
  exit 2
}

log() {
  echo "[$(date +%H:%M:%S)] $*"
}

cluster_up() {
  mkdir -p "${HOST_CHECKPOINTS}"
  if [[ -z "${KUBECONFIG_OUT}" ]]; then
    echo "KUBECONFIG or SNAPSHOT_E2E_KUBECONFIG is required" >&2
    exit 1
  fi

  if k3d cluster list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "${CLUSTER_NAME}"; then
    log "deleting leftover k3d cluster ${CLUSTER_NAME}"
    k3d cluster delete "${CLUSTER_NAME}"
  fi

  log "creating k3d cluster ${CLUSTER_NAME}"
  k3d cluster create "${CLUSTER_NAME}" \
    --wait \
    --kubeconfig-update-default=false \
    --kubeconfig-switch-context=false \
    --k3s-arg '--disable=traefik@server:0' \
    --volume "${HOST_CHECKPOINTS}:${NODE_CHECKPOINTS}@server:0"

  mkdir -p "$(dirname "${KUBECONFIG_OUT}")"
  k3d kubeconfig get "${CLUSTER_NAME}" > "${KUBECONFIG_OUT}"
  chmod 0600 "${KUBECONFIG_OUT}"
  export KUBECONFIG="${KUBECONFIG_OUT}"

  log "labeling k3d nodes for Snapshot agent + CPU test pods"
  kubectl get nodes -o name | while read -r node; do
    kubectl label "${node}" --overwrite \
      nvidia.com/gpu.present=true \
      nvidia.com/mig.config=all-disabled
  done

  log "ensuring RuntimeClass nvidia uses handler runc (k3s may already define nvidia; handler is immutable)"
  kubectl delete runtimeclass nvidia --ignore-not-found
  kubectl create -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: runc
EOF

  log "creating namespace ${TEST_NAMESPACE} and hostPath RWX PVC"
  kubectl create namespace "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
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
  storageClassName: snapshot-e2e-hostpath
  hostPath:
    path: ${NODE_CHECKPOINTS}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: snapshot-e2e-hostpath
  resources:
    requests:
      storage: ${PVC_SIZE}
  volumeName: snapshot-e2e-checkpoints
EOF

  kubectl wait --for=jsonpath='{.status.phase}'=Bound \
    "pvc/${PVC_NAME}" -n "${TEST_NAMESPACE}" --timeout=60s
  log "cluster ${CLUSTER_NAME} is ready (kubeconfig=${KUBECONFIG_OUT})"
}

cluster_down() {
  k3d cluster delete "${CLUSTER_NAME}" || true
}

cmd="${1:-}"
case "${cmd}" in
  cluster-up) cluster_up ;;
  cluster-down) cluster_down ;;
  *) usage ;;
esac
