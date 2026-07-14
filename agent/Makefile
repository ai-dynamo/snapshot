.PHONY: build test tidy lint fmt

build:
	go build -o bin/agent ./cmd/agent
	go build -o bin/nsrestore ./cmd/nsrestore

test:
	go test ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
