# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := test

.PHONY: build fmt lint tidy test test-integration

# Builds snapshot-coordinator, and compiles every other package too, such as
# the session client, so CodeQL, which analyzes only what the build compiles,
# covers the whole module.
build:
	go build ./...
	go build -o bin/snapshot-coordinator ./cmd/snapshot-coordinator

fmt:
	gofmt -w .

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

tidy:
	go mod tidy

# -race costs about half again the run time, and the coordinator's concurrency
# is what most needs it.
test:
	go test -race ./...

test-integration: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -tags=integration ./...
