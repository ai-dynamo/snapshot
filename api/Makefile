REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := generate

.PHONY: generate fmt lint tidy test

# The chart needs its own copy for Helm to install CRDs on a fresh release, so
# it is mirrored from the generated output rather than generated separately.
CHART_CRD_DIR := ../charts/snapshot/crds

generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object:headerFile=../hack/boilerplate.go.txt paths=./...
	$(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=./v1alpha1/crds
	rm -f $(CHART_CRD_DIR)/*.yaml
	cp v1alpha1/crds/*.yaml $(CHART_CRD_DIR)/

fmt:
	gofmt -w .

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

tidy:
	go mod tidy

test:
	go test ./...
