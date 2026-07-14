.PHONY: generate fmt lint tidy test

generate:
	controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./...
	controller-gen crd paths=./... output:crd:artifacts:config=../charts/snapshot/crds

fmt:
	gofmt -w .

lint:
	golangci-lint run --timeout=5m ./...

tidy:
	go mod tidy

test:
	go test ./...
