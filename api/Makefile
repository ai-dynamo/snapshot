REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := generate

.PHONY: generate fmt lint tidy test

generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object:headerFile=../hack/boilerplate.go.txt paths=./...
	$(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=../charts/snapshot/crds

fmt:
	gofmt -w .

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

tidy:
	go mod tidy

test:
	go test ./...
