.PHONY: build test tidy lint fmt

build:
	go build ./...

test:
	go test ./... -v

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
