GO_VERSION             ?= 1.26.6

CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION  ?= v1.62.2
ADDLICENSE_VERSION     ?= v1.1.1
GOVULNCHECK_VERSION    ?= v1.1.4
HELM_VERSION           ?= v3.17.3
# Protobuf's v21.12 release reports itself as libprotoc 3.21.12.
override PROTOC_VERSION := 3.21.12
PROTOC_RELEASE := 21.12

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

# Each tool installs on demand (only when its binary is missing), so targets can
# depend on it as a prerequisite without a separate install step in CI.
$(CONTROLLER_GEN):
	GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(GOLANGCI_LINT):
	GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(ADDLICENSE):
	GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install github.com/google/addlicense@$(ADDLICENSE_VERSION)

$(GOVULNCHECK):
	GOBIN=$(TOOLS_BIN_DIR) GOWORK=off go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

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

.PHONY: install-tools
install-tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ADDLICENSE) $(GOVULNCHECK) $(HELM)
