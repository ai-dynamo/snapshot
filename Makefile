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
        verify-license-headers govulncheck helm-lint docker-build-agent docker-build-operator

# controller-gen writes the CRDs to CRD_SRC_DIR, where the api module embeds
# them; api/Makefile mirrors that output into the chart so Helm can install them
# on a fresh release.
CRD_SRC_DIR   := api/v1alpha1/crds
CHART_CRD_DIR := charts/snapshot/crds

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

# api/v1alpha1/crds holds controller-gen output; a license header there would be
# stripped by the next generate and fail the clean-tree assert in check.
LICENSE_IGNORES := -ignore '**/zz_generated*.go' -ignore '**/.gitkeep' \
                   -ignore 'charts/**' -ignore 'api/v1alpha1/crds/**'

add-license-headers: $(ADDLICENSE)
	$(ADDLICENSE) -f hack/boilerplate.addlicense.txt $(LICENSE_IGNORES) . .github/workflows

verify-license-headers: $(ADDLICENSE)
	$(ADDLICENSE) -f hack/boilerplate.addlicense.txt -check $(LICENSE_IGNORES) . .github/workflows

# Assert the chart's CRD copy still matches the generated source. Deliberately
# ordered before generate in check and verify-generate: afterwards generate has
# repaired any drift and the comparison proves nothing. Needs no tooling and no
# Go test run, so it also stands alone on a fresh checkout.
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

docker-build-agent:
	docker buildx build $(DOCKER_BUILD_ARGS) -f agent/Dockerfile \
	  --build-context=api=./api --target agent \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/agent:$(t)) agent/

docker-build-operator:
	docker buildx build $(DOCKER_BUILD_ARGS) -f operator/Dockerfile \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/operator:$(t)) .
