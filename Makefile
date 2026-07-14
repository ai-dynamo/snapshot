.PHONY: tidy generate test build lint verify-generate

tidy:
	$(MAKE) -C api tidy
	$(MAKE) -C agent tidy
	$(MAKE) -C operator tidy
	$(MAKE) -C snapshotctl tidy

generate:
	$(MAKE) -C api generate

test:
	$(MAKE) -C agent test
	$(MAKE) -C operator test

build:
	$(MAKE) -C agent build
	$(MAKE) -C operator build

lint:
	$(MAKE) -C api lint
	$(MAKE) -C agent lint
	$(MAKE) -C operator lint
	$(MAKE) -C snapshotctl lint

verify-generate: generate
	git diff --exit-code
