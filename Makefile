include hack/tools.mk

.DEFAULT_GOAL := check

# check mutates files across stages (generate, license, fmt, tidy); running
# them in parallel would let lint observe partially-rewritten files.
.NOTPARALLEL:

# Image / chart publishing knobs (overridden by the publish workflows).
REGISTRY          ?= ghcr.io/ai-dynamo/snapshot
VERSION           ?= latest
TAGS              ?= $(VERSION)
CHART_VERSION     ?= $(VERSION)
APP_VERSION       ?= $(VERSION)
DOCKER_BUILD_ARGS ?=

.PHONY: tidy generate test build lint verify-generate check fmt add-license-headers \
        govulncheck helm-lint docker-build-agent docker-build-operator chart-package chart-push

tidy:
	$(MAKE) -C api tidy
	$(MAKE) -C agent tidy
	$(MAKE) -C operator tidy
	$(MAKE) -C snapshotctl tidy

generate: $(CONTROLLER_GEN)
	$(MAKE) -C api generate

test:
	$(MAKE) -C api test
	$(MAKE) -C agent test
	$(MAKE) -C operator test
	$(MAKE) -C snapshotctl test

build:
	$(MAKE) -C agent build
	$(MAKE) -C operator build
	$(MAKE) -C snapshotctl build

lint: $(GOLANGCI_LINT)
	$(MAKE) -C api lint
	$(MAKE) -C agent lint
	$(MAKE) -C operator lint
	$(MAKE) -C snapshotctl lint

fmt:
	$(MAKE) -C api fmt
	$(MAKE) -C agent fmt
	$(MAKE) -C operator fmt
	$(MAKE) -C snapshotctl fmt

add-license-headers: $(ADDLICENSE)
	$(ADDLICENSE) -c "NVIDIA Corporation" -l apache \
	  -ignore '**/zz_generated*.go' -ignore '**/.gitkeep' -ignore 'charts/**' \
	  . .github/workflows

# install-tools makes controller-gen/golangci-lint/addlicense/helm available to
# the stages before they run. govulncheck + helm-lint are read-only, so they run
# after the mutating stages and before the clean-tree assert.
check: install-tools generate add-license-headers fmt tidy lint govulncheck helm-lint
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: tree dirty after check — commit the changes below"; git status --porcelain; git diff; exit 1)

verify-generate: generate
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: generated files out of date — run 'make generate' and commit"; git status --porcelain; git diff; exit 1)

govulncheck: $(GOVULNCHECK)
	for m in api agent operator snapshotctl; do (cd $$m && $(GOVULNCHECK) ./...); done

helm-lint: $(HELM)
	$(HELM) lint charts/snapshot/

docker-build-agent:
	docker buildx build $(DOCKER_BUILD_ARGS) -f agent/Dockerfile \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/agent:$(t)) .

docker-build-operator:
	docker buildx build $(DOCKER_BUILD_ARGS) -f operator/Dockerfile \
	  $(foreach t,$(TAGS),-t $(REGISTRY)/operator:$(t)) .

chart-package: $(HELM)
	$(HELM) package charts/snapshot --version $(CHART_VERSION) --app-version $(APP_VERSION)

chart-push: $(HELM)
	$(HELM) push snapshot-$(CHART_VERSION).tgz oci://$(REGISTRY)/charts
