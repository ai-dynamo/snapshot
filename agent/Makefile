REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build test tidy lint fmt pagebroker-generate pagebroker-check-generated

PROTOC_GEN_GO := $(TOOLS_BIN_DIR)/protoc-gen-go

build:
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent
	GOOS=linux GOARCH=amd64 go build -o bin/nsrestore ./cmd/nsrestore

test:
	go test ./...

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
