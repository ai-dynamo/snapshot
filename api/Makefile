REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := generate

.PHONY: generate fmt lint tidy test

# Mirrored, not generated separately, so Helm can install CRDs on a fresh release.
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
