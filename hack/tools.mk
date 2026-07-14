CONTROLLER_GEN_VERSION ?= v0.16.0
GOLANGCI_LINT_VERSION  ?= v1.62.2

.PHONY: install-tools
install-tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
