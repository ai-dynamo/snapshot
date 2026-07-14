CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION  ?= v1.62.2
ADDLICENSE_VERSION     ?= v1.1.1
GOVULNCHECK_VERSION    ?= v1.1.4

# Install to an explicit GOBIN so callers can resolve the pinned binaries
# deterministically instead of picking up an older copy from PATH.
GOBIN ?= $(shell go env GOPATH)/bin

.PHONY: install-tools
install-tools:
	GOBIN=$(GOBIN) GOWORK=off go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	GOBIN=$(GOBIN) GOWORK=off go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN=$(GOBIN) GOWORK=off go install github.com/google/addlicense@$(ADDLICENSE_VERSION)
	GOBIN=$(GOBIN) GOWORK=off go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
