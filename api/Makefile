REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := generate

.PHONY: generate fmt lint tidy test

# CRDs are generated into v1alpha1/crds, where they are embedded into the api
# module and applied by the operator's init container. The chart needs its own
# copy under crds/ for Helm to install them on a fresh release, so that is
# mirrored from the same output rather than generated separately. `make check`
# fails on a dirty tree, which is what keeps the two in step.
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
