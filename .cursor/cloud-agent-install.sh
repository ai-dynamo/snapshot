#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Idempotent Cloud Agent bootstrap for the ai-dynamo Snapshot + Grove team environment.

set -euo pipefail

GO_VERSION="${GO_VERSION:-1.27.3}"
TOOLS_BIN="${TOOLS_BIN:-/home/ubuntu/go/bin}"
export PATH="${TOOLS_BIN}:${HOME}/.local/bin:/usr/local/bin:${PATH}"

ensure_go() {
  if command -v go >/dev/null 2>&1 && go version | grep -q "go${GO_VERSION} "; then
    return 0
  fi
  tmp="$(mktemp)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o "${tmp}"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "${tmp}"
  rm -f "${tmp}"
  sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
}

find_repo() {
  local name="$1"
  local candidate
  for candidate in \
    "/agent/repos/${name}" \
    "${HOME}/repos/${name}" \
    "$(dirname "${BASH_SOURCE[0]}")/.." \
    "$(dirname "${BASH_SOURCE[0]}")/../../${name}"; do
    if [[ -d "${candidate}" && -f "${candidate}/Makefile" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

ensure_uv() {
  if command -v uv >/dev/null 2>&1; then
    return 0
  fi
  curl -fsSL https://astral.sh/uv/install.sh | sh
  export PATH="${HOME}/.local/bin:${PATH}"
}

install_grove_tools() {
  local grove_root="$1"
  local grove_tools="${grove_root}/hack/tools/bin"
  mkdir -p "${grove_tools}"
  if [[ ! -x "${grove_tools}/kind" ]]; then
    curl -fsSL -o "${grove_tools}/kind" \
      "https://github.com/kubernetes-sigs/kind/releases/download/v0.30.0/kind-linux-amd64"
    chmod +x "${grove_tools}/kind"
  fi
  if [[ ! -x "${grove_tools}/yq" ]]; then
    curl -fsSL -o "${grove_tools}/yq" \
      "https://github.com/mikefarah/yq/releases/download/v4.48.1/yq_linux_amd64"
    chmod +x "${grove_tools}/yq"
  fi
  if [[ ! -x "${grove_tools}/golangci-lint" ]] \
    || ! "${grove_tools}/golangci-lint" version 2>/dev/null | grep -q 'go1.27'; then
    GOBIN="${grove_tools}" GOTOOLCHAIN="go${GO_VERSION}" \
      go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.1
  fi
}

ensure_go
ensure_uv

SNAPSHOT_ROOT="$(find_repo snapshot)"
GROVE_ROOT="$(find_repo grove || true)"

if [[ -z "${SNAPSHOT_ROOT}" ]]; then
  echo "snapshot repository not found" >&2
  exit 1
fi

make -C "${SNAPSHOT_ROOT}" install-tools
(
  cd "${SNAPSHOT_ROOT}"
  go work sync
  make -C api tidy
  make -C agent tidy
  make -C operator tidy
)

if [[ -n "${GROVE_ROOT}" ]]; then
  install_grove_tools "${GROVE_ROOT}"
  (
    cd "${GROVE_ROOT}"
    make tidy
  )
  export PATH="${GROVE_ROOT}/hack/tools/bin:${PATH}"
fi

(
  cd "${SNAPSHOT_ROOT}/e2e"
  uv sync
)

# kubectl for Snapshot e2e helpers (pinned loosely to stable v1.31).
if ! command -v kubectl >/dev/null 2>&1; then
  kubectl_ver="v1.31.0"
  curl -fsSL -o /tmp/kubectl \
    "https://dl.k8s.io/release/${kubectl_ver}/bin/linux/amd64/kubectl"
  chmod +x /tmp/kubectl
  sudo install -m 0755 /tmp/kubectl /usr/local/bin/kubectl
  rm -f /tmp/kubectl
fi

# Login shells do not load ~/go/bin; publish pinned tools on the default PATH.
publish_bin() {
  local src="$1"
  local name
  name="$(basename "${src}")"
  if [[ -x "${src}" ]]; then
    sudo ln -sf "${src}" "/usr/local/bin/${name}"
  fi
}
for tool in helm golangci-lint controller-gen addlicense govulncheck protoc mdtoc setup-envtest; do
  publish_bin "${TOOLS_BIN}/${tool}"
done
if [[ -n "${GROVE_ROOT:-}" ]]; then
  publish_bin "${GROVE_ROOT}/hack/tools/bin/kind"
  publish_bin "${GROVE_ROOT}/hack/tools/bin/yq"
  publish_bin "${GROVE_ROOT}/hack/tools/bin/golangci-lint"
fi
sudo ln -sf "${HOME}/.local/bin/uv" /usr/local/bin/uv 2>/dev/null || true

echo "Cloud Agent install finished (snapshot=${SNAPSHOT_ROOT}${GROVE_ROOT:+, grove=${GROVE_ROOT}})"
