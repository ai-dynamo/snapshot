# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ROOT := ../..
AGENT_PLATFORM ?= linux/amd64
REGISTRY ?= local
VERSION ?= integration
BUILD_IMAGE := $(REGISTRY)/pagebroker:$(VERSION)-dependencies
RUNTIME_IMAGE := $(REGISTRY)/pagebroker:$(VERSION)
TEST_IMAGE := $(REGISTRY)/pagebroker:$(VERSION)-integration
VENV := .venv

.PHONY: test images checkpoint-test
# Run from the repository root with make pagebroker-integration-test.
test: images $(VENV)/.installed
	$(VENV)/bin/python3 s3_integration.py --image "$(TEST_IMAGE)" --resources
	$(VENV)/bin/python3 s3_integration.py --image "$(TEST_IMAGE)" --tls
	$(MAKE) checkpoint-test

images:
	$(MAKE) -C $(ROOT) docker-build-pagebroker REGISTRY="$(REGISTRY)" VERSION="$(VERSION)" DOCKER_BUILD_ARGS="$(DOCKER_BUILD_ARGS) --load"
	$(MAKE) -C $(ROOT) docker-build-pagebroker REGISTRY="$(REGISTRY)" VERSION="$(VERSION)-dependencies" DOCKER_BUILD_ARGS="$(DOCKER_BUILD_ARGS) --target build-dependencies --load"
	cd $(ROOT) && docker buildx build --load --platform "$(AGENT_PLATFORM)" -f hack/pagebroker-integration/Dockerfile \
		--build-arg BUILD_IMAGE="$(BUILD_IMAGE)" --build-arg RUNTIME_IMAGE="$(RUNTIME_IMAGE)" -t "$(TEST_IMAGE)" .

$(VENV)/.installed: requirements.txt
	python3 -m venv $(VENV)
	$(VENV)/bin/python3 -m pip install --disable-pip-version-check -r $<
	touch $@

checkpoint-test: $(VENV)/.installed
	cd $(ROOT)/agent && go test -tags=integration -c ./internal/pagebroker -o ../hack/pagebroker-integration/checkpoint-client.test
	$(VENV)/bin/python3 checkpoint_integration.py --image "$(RUNTIME_IMAGE)" --client ./checkpoint-client.test
	$(VENV)/bin/python3 checkpoint_integration.py --image "$(RUNTIME_IMAGE)" --client ./checkpoint-client.test --tls
