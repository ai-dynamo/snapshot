REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

CXX ?= g++
CUDA_HELPER_CXXFLAGS := -std=c++20 -O2 -Wall -Wextra -Werror -pthread
CUDA_HELPER_TESTS_AVAILABLE := $(shell \
	probe=$$(mktemp); \
	printf '\043include <openssl/evp.h>\n\043include <sys/signalfd.h>\nint main() { return 0; }\n' | \
		$(CXX) $(CUDA_HELPER_CXXFLAGS) -x c++ - -lcrypto -o "$$probe" \
		>/dev/null 2>&1; \
	status=$$?; rm -f "$$probe"; \
	test $$status -eq 0 && printf 1)
REQUIRE_CUDA_HELPER_TESTS ?= $(if $(CI),1,0)

.DEFAULT_GOAL := build

.PHONY: build test test-go test-cuda-helper tidy lint fmt

build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

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

test-cuda-helper:
	@mkdir -p bin
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-daemon-protocol-test \
		cmd/cuda-checkpoint-helper/daemon_protocol.cpp \
		cmd/cuda-checkpoint-helper/daemon_protocol_test.cpp
	bin/cuda-checkpoint-helper-daemon-protocol-test
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-storage-test \
		cmd/cuda-checkpoint-helper/content_digest.cpp \
		cmd/cuda-checkpoint-helper/storage_manifest.cpp \
		cmd/cuda-checkpoint-helper/storage_manifest_test.cpp \
		-lcrypto
	bin/cuda-checkpoint-helper-storage-test
	$(CXX) $(CUDA_HELPER_CXXFLAGS) \
		-o bin/cuda-checkpoint-helper-content-digest-test \
		cmd/cuda-checkpoint-helper/content_digest.cpp \
		cmd/cuda-checkpoint-helper/content_digest_test.cpp \
		-lcrypto
	bin/cuda-checkpoint-helper-content-digest-test
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
