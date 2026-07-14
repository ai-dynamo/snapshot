.PHONY: generate fmt lint tidy test

generate:
	controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./...

fmt:
	gofmt -w .

lint:
	golangci-lint run --timeout=5m ./...

tidy:
	go mod tidy

test:
	go test ./...
