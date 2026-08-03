include hack/tools.mk

.DEFAULT_GOAL := check

# check mutates files across stages (generate, license, fmt, tidy); running
# them in parallel would let lint observe partially-rewritten files.
.NOTPARALLEL:

# Image / chart publishing knobs (overridden by the publish workflows).
REGISTRY          ?= ghcr.io/ai-dynamo/snapshot
VERSION           ?= latest
TAGS              ?= $(VERSION)
DOCKER_BUILD_ARGS ?=

.PHONY: tidy generate test build lint verify-generate verify-crds check fmt add-license-headers \
        verify-license-headers govulncheck helm-lint docker-build-agent docker-build-operator \
        linux-build linux-test

CRD_SRC_DIR   := api/v1alpha1/crds
CHART_CRD_DIR := charts/snapshot/crds

LINUX_GO_IMAGE ?= golang:$(GO_VERSION)

tidy:
	$(MAKE) -C api tidy
	$(MAKE) -C agent tidy
	$(MAKE) -C operator tidy

generate: $(CONTROLLER_GEN)
	$(MAKE) -C api generate

test:
	$(MAKE) -C api test
	$(MAKE) -C agent test
	$(MAKE) -C operator test

build:
	$(MAKE) -C agent build
	$(MAKE) -C operator build

lint: $(GOLANGCI_LINT)
	$(MAKE) -C api lint
	$(MAKE) -C agent lint
	$(MAKE) -C operator lint

fmt:
	$(MAKE) -C api fmt
	$(MAKE) -C agent fmt
	$(MAKE) -C operator fmt

# A license header on controller-gen output would be stripped by the next generate,
# so both CRD copies are exempt. The rest of the chart carries SPDX headers.
LICENSE_IGNORES := -ignore '**/zz_generated*.go' -ignore '**/.gitkeep' \
                   -ignore '$(CHART_CRD_DIR)/**' -ignore '$(CRD_SRC_DIR)/**'

add-license-headers: $(ADDLICENSE)
	$(ADDLICENSE) -f hack/boilerplate.addlicense.txt $(LICENSE_IGNORES) . .github/workflows

verify-license-headers: $(ADDLICENSE)
	$(ADDLICENSE) -f hack/boilerplate.addlicense.txt -check $(LICENSE_IGNORES) . .github/workflows

# Ordered before generate: afterwards it would compare freshly repaired copies.
verify-crds:
	@diff -r -x '*.go' $(CRD_SRC_DIR) $(CHART_CRD_DIR) || \
	  (echo "ERROR: $(CHART_CRD_DIR) has drifted from $(CRD_SRC_DIR) — run 'make generate' and commit"; exit 1)

# install-tools makes controller-gen/golangci-lint/addlicense/helm available to
# the stages before they run. govulncheck + helm-lint are read-only, so they run
# after the mutating stages and before the clean-tree assert.
check: verify-crds install-tools generate add-license-headers fmt tidy verify-license-headers lint govulncheck helm-lint
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: tree dirty after check — commit the changes below"; git status --porcelain; git diff; exit 1)

verify-generate: verify-crds generate
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: generated files out of date — run 'make generate' and commit"; git status --porcelain; git diff; exit 1)

govulncheck: $(GOVULNCHECK)
	for m in api agent operator; do (cd $$m && $(GOVULNCHECK) ./...) || exit 1; done

helm-lint: $(HELM)
	$(HELM) lint charts/snapshot/

# Run build/test inside a Linux container (local dev only; CI runs on Linux natively).
linux-build:
	docker run --rm \
	  --user "$$(id -u):$$(id -g)" \
	  -e HOME=/tmp -e GOCACHE=/tmp/go-build \
	  -v "$(CURDIR):/workspace" -w /workspace \
	  $(LINUX_GO_IMAGE) \
	  make -C agent build

linux-test:
	docker run --rm \
	  --user "$$(id -u):$$(id -g)" \
	  -e HOME=/tmp -e GOCACHE=/tmp/go-build \
	  -v "$(CURDIR):/workspace" -w /workspace \
	  $(LINUX_GO_IMAGE) \
	  make -C agent test

docker-build-agent:
	docker buildx build $(DOCKER_BUILD_ARGS) -f agent/Dockerfile \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-context=api=./api --target agent \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/agent:$(t)) agent/

docker-build-operator:
	docker buildx build $(DOCKER_BUILD_ARGS) -f operator/Dockerfile \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/operator:$(t)) .
