# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

GO_VERSION             ?= 1.27.4

CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION  ?= v2.14.0
ADDLICENSE_VERSION     ?= v1.1.1
GOVULNCHECK_VERSION    ?= v1.8.0
HELM_VERSION           ?= v3.17.3
# Protobuf's v21.12 release reports itself as libprotoc 3.21.12.
override PROTOC_VERSION := 3.21.12
PROTOC_RELEASE := 21.12
MDTOC_VERSION          ?= v1.4.0
SETUP_ENVTEST_VERSION  ?= v0.25.1
# Matches the k8s.io/api version operator/go.mod pins, so the fake apiserver
# understands the same schema the operator was built and tested against.
ENVTEST_K8S_VERSION    ?= 1.37.0

# Install tools into an explicit bin dir and put it ahead on PATH so callers
# (including submakes) resolve the pinned binaries. Defaults to GOPATH/bin.
TOOLS_BIN_DIR ?= $(shell go env GOPATH)/bin
export PATH := $(TOOLS_BIN_DIR):$(PATH)

# Host os/arch for downloading pre-built binaries (helm).
SYSTEM_NAME := $(shell uname -s | tr '[:upper:]' '[:lower:]')
SYSTEM_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

CONTROLLER_GEN := $(TOOLS_BIN_DIR)/controller-gen
GOLANGCI_LINT  := $(TOOLS_BIN_DIR)/golangci-lint
ADDLICENSE     := $(TOOLS_BIN_DIR)/addlicense
GOVULNCHECK    := $(TOOLS_BIN_DIR)/govulncheck
HELM           := $(TOOLS_BIN_DIR)/helm
PROTOC         := $(TOOLS_BIN_DIR)/protoc
MDTOC          := $(TOOLS_BIN_DIR)/mdtoc
SETUP_ENVTEST  := $(TOOLS_BIN_DIR)/setup-envtest

# A tool that type-checks our source has to be built with at least our Go
# version: golangci-lint refuses to start when it was built with an older one,
# and govulncheck panics part-way through. Both pin a toolchain in their own
# go.mod that lags ours, and `go install` honours that pin — so force ours.
# Without this, every GO_VERSION bump breaks `make check` until the tools
# catch up. Tools that only rewrite or generate files do not need it.
TOOL_TOOLCHAIN := GOTOOLCHAIN=go$(GO_VERSION)

# Each tool installs on demand, so targets can depend on it as a prerequisite
# without a separate install step in CI. A Go tool is rechecked on every
# invocation and reinstalled unless the binary is the pinned build: the default
# TOOLS_BIN_DIR outlives a version bump here and is shared with other
# checkouts, and a binary left from before the bump fails in ways that do not
# name the cause. The check costs one read of the binary.
.PHONY: force-tool-version-check
force-tool-version-check:

# go-tool-pinned succeeds when binary $(1) was built from module version $(2)
# and, when $(3) is given, by Go toolchain $(3). `go version -m` prints both
# from the binary itself, so there is no stamp file to fall out of step.
go-tool-pinned = test -x "$(1)" && go version -m "$(1)" 2>/dev/null | \
  awk -v version="$(2)" -v toolchain="$(3)" \
    'NR == 1 && toolchain != "" && $$NF != toolchain { stale = 1 } \
     $$1 == "mod" && $$3 == version { pinned = 1 } \
     END { exit stale || !pinned }'

$(CONTROLLER_GEN): force-tool-version-check
	@$(call go-tool-pinned,$@,$(CONTROLLER_GEN_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION); }

$(GOLANGCI_LINT): force-tool-version-check
	@$(call go-tool-pinned,$@,$(GOLANGCI_LINT_VERSION),go$(GO_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off $(TOOL_TOOLCHAIN) \
	  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); }

$(ADDLICENSE): force-tool-version-check
	@$(call go-tool-pinned,$@,$(ADDLICENSE_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install github.com/google/addlicense@$(ADDLICENSE_VERSION); }

$(GOVULNCHECK): force-tool-version-check
	@$(call go-tool-pinned,$@,$(GOVULNCHECK_VERSION),go$(GO_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off $(TOOL_TOOLCHAIN) \
	  go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); }

# helm ships as a tarball (<os>-<arch>/helm); extract just the binary.
$(HELM):
	mkdir -p $(TOOLS_BIN_DIR)
	curl -fsSL --retry 3 --retry-connrefused https://get.helm.sh/helm-$(HELM_VERSION)-$(SYSTEM_NAME)-$(SYSTEM_ARCH).tar.gz \
	  | tar -xzf - -C $(TOOLS_BIN_DIR) --strip-components=1 $(SYSTEM_NAME)-$(SYSTEM_ARCH)/helm

$(PROTOC):
	@set -e; mkdir -p $(TOOLS_BIN_DIR); tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	 curl -fsSL --retry 3 --connect-timeout 10 --max-time 120 https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_RELEASE)/protoc-$(PROTOC_RELEASE)-linux-x86_64.zip -o "$$tmp/protoc.zip"; \
 echo '3a4c1e5f2516c639d3079b1586e703fc7bcfa2136d58bda24d1d54f949c315e8  '"$$tmp/protoc.zip" | sha256sum -c -; \
	 unzip -q "$$tmp/protoc.zip" -d "$$tmp"; \
	 install -m 755 "$$tmp/bin/protoc" $(PROTOC)

.PHONY: protoc
protoc: $(PROTOC)
	@test "$$($(PROTOC) --version)" = "libprotoc $(PROTOC_VERSION)"

$(MDTOC): force-tool-version-check
	@$(call go-tool-pinned,$@,$(MDTOC_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install sigs.k8s.io/mdtoc@$(MDTOC_VERSION); }

$(SETUP_ENVTEST): force-tool-version-check
	@$(call go-tool-pinned,$@,$(SETUP_ENVTEST_VERSION)) || { set -x; \
	  GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION); }

.PHONY: install-tools
install-tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ADDLICENSE) $(GOVULNCHECK) $(HELM) $(MDTOC) $(SETUP_ENVTEST)
