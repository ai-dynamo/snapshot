# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := test

.PHONY: build fmt lint tidy test

# The module has no binary; make build compiles it so CodeQL, which analyzes
# only what the build compiles, covers it.
build:
	go build ./...

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
