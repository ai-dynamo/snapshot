.PHONY: build tidy lint

build:
	go build -o bin/snapshotctl ./cmd/snapshotctl

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
