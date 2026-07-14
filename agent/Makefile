.PHONY: build test tidy lint

build:
	go build ./...

test:
	go test ./... -v

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
