.PHONY: build test generate tidy lint fmt

build:
	go build ./...

test:
	go test ./... -v

fmt:
	gofmt -w .

generate:
	controller-gen object:headerFile="../hack/boilerplate.go.txt" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
