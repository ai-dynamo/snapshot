# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build go-build cuinterpose-build cuinterpose-test cuda-checkpoint-helper test test-go test-pagebroker tidy lint fmt pagebroker-generate pagebroker-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go
CUDA_ROOT ?= /usr/local/cuda

# Native artifacts have their own build and CI gate so Go-only work needs no Docker.
build: go-build

cuinterpose-build:
	$(MAKE) -C cmd/cuinterpose build

cuinterpose-test:
	$(MAKE) -C cmd/cuinterpose test

go-build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

# Conventional driver-managed checkpointing and state queries stay in this CLI.
cuda-checkpoint-helper:
	$(MAKE) -C cmd/cuda-checkpoint-helper helper CUDA_ROOT=$(CUDA_ROOT)
	mkdir -p bin
	cp cmd/cuda-checkpoint-helper/bin/cuda-checkpoint-helper bin/

test: test-go

test-go:
	go test ./...

# Run the PageBroker CPU tests with the PageBroker image's build dependencies.
test-pagebroker:
	$(MAKE) -C pagebroker test CUDA_ROOT=$(CUDA_ROOT)

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
