# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

CXX ?= g++
CUDA_HELPER_CXXFLAGS := -std=c++20 -O2 -Wall -Wextra -Werror -pthread
CUDA_HELPER_TESTS_AVAILABLE := $(shell \
	probe=$$(mktemp); \
	printf 'int main() { return 0; }\n' | \
		$(CXX) $(CUDA_HELPER_CXXFLAGS) -x c++ - -o "$$probe" \
		>/dev/null 2>&1; \
	status=$$?; rm -f "$$probe"; \
	test $$status -eq 0 && printf 1)
REQUIRE_CUDA_HELPER_TESTS ?= $(if $(CI),1,0)

.DEFAULT_GOAL := build

.PHONY: build go-build cuinterpose-build cuinterpose-test test test-go test-cuda-helper tidy lint fmt pagebroker-generate pagebroker-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go

build: go-build cuinterpose-build

cuinterpose-build:
	$(MAKE) -C cmd/cuinterpose build

cuinterpose-test:
	$(MAKE) -C cmd/cuinterpose test

go-build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

test: test-go

test-go:
	go test ./...

ifeq ($(CUDA_HELPER_TESTS_AVAILABLE),1)
test: test-cuda-helper
else ifeq ($(REQUIRE_CUDA_HELPER_TESTS),1)
test:
	@echo "ERROR: C++20 compiler is required for CUDA helper tests in CI" >&2
	@exit 1
else
test:
	@echo "WARNING: skipping CUDA helper tests; run 'make test-cuda-helper'" \
		"after installing a C++20 compiler" >&2
endif

test-cuda-helper:
	@mkdir -p bin
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-storage-test \
		cmd/cuda-checkpoint-helper/storage_manifest.cpp \
		cmd/cuda-checkpoint-helper/storage_manifest_test.cpp
	bin/cuda-checkpoint-helper-storage-test
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-transfer-config-test \
		cmd/cuda-checkpoint-helper/transfer_config.cpp \
		cmd/cuda-checkpoint-helper/transfer_config_test.cpp
	bin/cuda-checkpoint-helper-transfer-config-test
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-transfer-cancellation-test \
		cmd/cuda-checkpoint-helper/transfer_engine_test.cpp
	bin/cuda-checkpoint-helper-transfer-cancellation-test

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
