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

# Base image for the agent, read from the Dockerfile so the digest lives in one
# place. capture-base-packages and docker-build-agent must agree on it, or the
# committed package baseline would describe a different image than we build on.
AGENT_BASE_IMAGE ?= $(shell sed -n 's/^ARG AGENT_BASE_IMAGE=//p' agent/Dockerfile)

# The agent is x86_64-only (cuda-checkpoint ships no other arch) and the package
# baseline is captured for this platform, so pin it rather than inheriting
# whatever the buildx builder defaults to.
AGENT_PLATFORM ?= linux/amd64

.PHONY: tidy generate test build lint verify-generate verify-crds check fmt add-license-headers \
        verify-license-headers govulncheck helm-lint docker-build-agent docker-build-operator capture-base-packages verify-base-packages \
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

# Refresh the agent's base-image package baseline. Run whenever AGENT_BASE_IMAGE
# changes; verify-base-packages fails the agent build if you forget.
capture-base-packages:
	@sh hack/capture-base-packages.sh "$(AGENT_BASE_IMAGE)" agent/compliance/base-packages.tsv "$(AGENT_PLATFORM)"
	@echo "baseline: $$(grep -vc '^#' agent/compliance/base-packages.tsv) packages"

# The source delta is computed against the committed baseline, so a base-image
# bump without a re-capture would silently skew it: packages the new base added
# would look like ours, and packages it dropped would vanish from the delta. The
# build would still succeed, with wrong compliance content. Fail instead.
verify-base-packages:
	@set -e; \
	tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	sh hack/capture-base-packages.sh "$(AGENT_BASE_IMAGE)" "$$tmp" "$(AGENT_PLATFORM)"; \
	diff -u agent/compliance/base-packages.tsv "$$tmp" || \
	  (echo "ERROR: agent/compliance/base-packages.tsv is stale for $(AGENT_BASE_IMAGE) — run 'make capture-base-packages' and commit"; exit 1)

docker-build-agent: verify-base-packages
	docker buildx build $(DOCKER_BUILD_ARGS) --platform "$(AGENT_PLATFORM)" -f agent/Dockerfile \
	  --build-arg "GO_VERSION=$(GO_VERSION)" \
	  --build-arg "AGENT_BASE_IMAGE=$(AGENT_BASE_IMAGE)" \
	  --build-context=api=./api --build-context=compliance=./hack/compliance \
	  --target agent \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/agent:$(t)) agent/

docker-build-operator:
	docker buildx build $(DOCKER_BUILD_ARGS) -f operator/Dockerfile \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/operator:$(t)) .
