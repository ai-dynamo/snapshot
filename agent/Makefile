# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

CXX ?= g++
CUDA_HELPER_CXXFLAGS := -std=c++20 -O2 -Wall -Wextra -Werror -pthread
CUDA_HELPER_TESTS_AVAILABLE := $(shell \
	probe=$$(mktemp); \
	printf '\043include <openssl/evp.h>\nint main() { return 0; }\n' | \
		$(CXX) $(CUDA_HELPER_CXXFLAGS) -x c++ - -lcrypto -o "$$probe" \
		>/dev/null 2>&1; \
	probe_status=$$?; rm -f "$$probe"; \
	test $$probe_status -eq 0 && printf 1)
REQUIRE_CUDA_HELPER_TESTS ?= $(if $(CI),1,0)

.DEFAULT_GOAL := build

.PHONY: build cuda-checkpoint-helper test test-go test-cuda-helper test-cuda-helper-storage tidy lint fmt pagebroker-generate pagebroker-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go
CUDA_ROOT ?= /usr/local/cuda

build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

# The persistent helper retains the existing one-shot CLI entrypoints.
cuda-checkpoint-helper:
	$(MAKE) -C cmd/cuda-checkpoint-helper helper CUDA_ROOT=$(CUDA_ROOT) NIXL_ROOT=$(NIXL_ROOT)
	mkdir -p bin
	cp cmd/cuda-checkpoint-helper/bin/cuda-checkpoint-helper bin/

test: test-go

test-go:
	go test ./...

ifeq ($(CUDA_HELPER_TESTS_AVAILABLE),1)
test: test-cuda-helper
else ifeq ($(REQUIRE_CUDA_HELPER_TESTS),1)
test:
	@echo "ERROR: C++20 compiler and OpenSSL headers/libraries are required for CUDA helper tests in CI" >&2
	@exit 1
else
test:
	@echo "WARNING: skipping CUDA helper tests; run 'make test-cuda-helper'" \
		"after installing a C++20 compiler and OpenSSL development files" >&2
endif

test-cuda-helper: test-cuda-helper-storage

test-cuda-helper-storage:
	$(MAKE) -C cmd/cuda-checkpoint-helper test-storage

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

pagebroker-generate: protoc $(PROTOC_GEN_GO)
	PATH="$(TOOLS_BIN_DIR):$$PATH" go generate ./internal/pagebroker

pagebroker-check-generated: pagebroker-generate
	git diff --exit-code -- internal/pagebroker/pagebroker.pb.go


$(PROTOC_GEN_GO): go.mod
	GOBIN=$(TOOLS_BIN_DIR) go install tool
