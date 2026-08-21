REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build test test-cuda-helper tidy lint fmt

build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

test: test-cuda-helper
	go test ./...

test-cuda-helper:
	@mkdir -p bin
	g++ -std=c++20 -O2 -Wall -Wextra -Werror -pthread \
		-o bin/cuda-checkpoint-helper-daemon-protocol-test \
		cmd/cuda-checkpoint-helper/daemon_protocol.cpp \
		cmd/cuda-checkpoint-helper/daemon_protocol_test.cpp
	bin/cuda-checkpoint-helper-daemon-protocol-test
	g++ -std=c++20 -O2 -Wall -Wextra -Werror \
		-o bin/cuda-checkpoint-helper-storage-test \
		cmd/cuda-checkpoint-helper/storage_manifest.cpp \
		cmd/cuda-checkpoint-helper/storage_manifest_test.cpp
	bin/cuda-checkpoint-helper-storage-test
	g++ -std=c++20 -O2 -Wall -Wextra -Werror \
		-o bin/cuda-checkpoint-helper-transfer-test \
		cmd/cuda-checkpoint-helper/transfer_config.cpp \
		cmd/cuda-checkpoint-helper/transfer_config_test.cpp
	bin/cuda-checkpoint-helper-transfer-test

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...
