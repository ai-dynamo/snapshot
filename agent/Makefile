# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build cuda-checkpoint-helper test test-go test-pagebroker-storage tidy lint fmt pagebroker-generate pagebroker-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go
CUDA_ROOT ?= /usr/local/cuda

build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

# Ordinary CUDA helper: no PageBroker runtime, CustomStorage headers, or NIXL.
cuda-checkpoint-helper:
	mkdir -p bin
	$(CC) -O2 -Wall -Wextra -Werror -I. -I$(CUDA_ROOT)/include \
		-L$(CUDA_ROOT)/lib64/stubs -lcuda -o bin/cuda-checkpoint-helper

test: test-go

test-go:
	go test ./...

test: test-pagebroker-storage

test-pagebroker-storage:
	$(MAKE) -C pagebroker test-storage

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
