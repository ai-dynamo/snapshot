CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION  ?= v1.62.2
ADDLICENSE_VERSION     ?= v1.1.1
GOVULNCHECK_VERSION    ?= v1.1.4

# Install tools into an explicit bin dir and put it ahead on PATH so callers
# (including submakes) resolve the pinned binaries. Defaults to GOPATH/bin.
TOOLS_BIN_DIR ?= $(shell go env GOPATH)/bin
export PATH := $(TOOLS_BIN_DIR):$(PATH)

CONTROLLER_GEN := $(TOOLS_BIN_DIR)/controller-gen
GOLANGCI_LINT  := $(TOOLS_BIN_DIR)/golangci-lint
ADDLICENSE     := $(TOOLS_BIN_DIR)/addlicense
GOVULNCHECK    := $(TOOLS_BIN_DIR)/govulncheck

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

.PHONY: install-tools
install-tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ADDLICENSE) $(GOVULNCHECK)
