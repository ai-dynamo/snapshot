# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build go-build cuinterpose-build cuinterpose-test cuda-checkpoint-helper test test-go test-pagebroker tidy lint fmt proto-generate proto-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go
PROTOC_GEN_GO_GRPC := $(TOOLS_BIN_DIR)/protoc-gen-go-grpc

# Every package generated from a .proto, and the files generation writes. Both
# plugins are pinned as tool directives in go.mod, and protoc in tools.mk, so
# every host generates the same bytes.
PROTO_PACKAGES := ./internal/pagebroker ./pkg/coordinator/v1alpha1
PROTO_GENERATED := internal/pagebroker/pagebroker.pb.go \
	pkg/coordinator/v1alpha1/coordinator.pb.go \
	pkg/coordinator/v1alpha1/coordinator_grpc.pb.go
CUDA_ROOT ?= /usr/local/cuda
GOARCH ?= amd64

# Native artifacts have their own build and CI gate so Go-only work needs no Docker.
build: go-build

cuinterpose-build:
	$(MAKE) -C cmd/cuinterpose build

cuinterpose-test:
	$(MAKE) -C cmd/cuinterpose test

go-build:
	GOOS=linux GOARCH=$(GOARCH) go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=$(GOARCH) go build -o bin/nsrestore ./cmd/nsrestore

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

proto-generate: protoc $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)
	PATH="$(TOOLS_BIN_DIR):$$PATH" go generate $(PROTO_PACKAGES)

# git status rather than git diff, so a generated file that was never committed
# fails the check too.
proto-check-generated: proto-generate
	@test -z "$$(git status --porcelain -- $(PROTO_GENERATED))" || \
	  (echo "ERROR: generated protobuf code is stale — run 'make -C agent proto-generate' and commit"; \
	   git status --porcelain -- $(PROTO_GENERATED); git diff -- $(PROTO_GENERATED); exit 1)


# go install tool builds every tool directive at once; each plugin names the
# same recipe so either one missing rebuilds both.
$(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC): go.mod
	GOBIN=$(TOOLS_BIN_DIR) go install tool
