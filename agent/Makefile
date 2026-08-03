REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := build

.PHONY: build test tidy lint fmt

build: pagebroker-build
	GOOS=linux GOARCH=amd64 go build -o bin/agent ./cmd/agent

pagebroker-build:
	$(MAKE) -C pagebroker

test: pagebroker-test
	go test ./...

pagebroker-test:
	$(MAKE) -C pagebroker test

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...
