.PHONY: build test generate tidy lint

build:
	go build ./...

test:
	go test ./... -v

generate:
	controller-gen object:headerFile="../hack/boilerplate.go.txt" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd

tidy:
	go mod tidy

lint:
	golangci-lint run --timeout=5m ./...
