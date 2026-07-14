# Ensure Go-installed tools (controller-gen, golangci-lint, addlicense,
# govulncheck) resolve from GOBIN regardless of the caller's PATH ordering.
GOBIN ?= $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# check mutates files across stages (generate, license, fmt, tidy); running
# them in parallel would let lint observe partially-rewritten files.
.NOTPARALLEL:

.PHONY: tidy generate test build lint verify-generate check fmt add-license-headers

tidy:
	$(MAKE) -C api tidy
	$(MAKE) -C agent tidy
	$(MAKE) -C operator tidy
	$(MAKE) -C snapshotctl tidy

generate:
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

lint:
	$(MAKE) -C api lint
	$(MAKE) -C agent lint
	$(MAKE) -C operator lint
	$(MAKE) -C snapshotctl lint

fmt:
	$(MAKE) -C api fmt
	$(MAKE) -C agent fmt
	$(MAKE) -C operator fmt
	$(MAKE) -C snapshotctl fmt

add-license-headers:
	addlicense -c "NVIDIA Corporation" -l apache \
	  -ignore '**/zz_generated*.go' -ignore '**/.gitkeep' -ignore 'charts/**' \
	  . .github/workflows

check: generate add-license-headers fmt tidy lint
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: tree dirty after check — commit the changes below"; git status --porcelain; git diff; exit 1)

verify-generate: generate
	@test -z "$$(git status --porcelain)" || \
	  (echo "ERROR: generated files out of date — run 'make generate' and commit"; git status --porcelain; git diff; exit 1)
