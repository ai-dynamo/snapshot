# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build test test-integration tidy lint fmt

build:
	go build -o bin/manager ./cmd/manager
	go build -o bin/crd-installer ./cmd/crdinstaller

test:
	go test ./...

# Runs admission tests requiring a real apiserver (envtest kube-apiserver/etcd),
# gated behind the integration tag so plain `go test ./...`/`make test` stays
# fast and dependency-free.
test-integration: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -tags=integration ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...
