# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build test frontend test-native

CBINDGEN ?= cbindgen
CUDA_INCLUDE ?= /opt/cuda/include

build:
	docker buildx build --platform linux/amd64 --target cuinterpose-export \
		--output type=local,dest=build -f ../../Dockerfile ../..

test:
	docker buildx build --platform linux/amd64 --target cuinterpose-test \
		-f ../../Dockerfile ../..

frontend:
	mkdir -p build
	cd rust && $(CBINDGEN) --quiet --config abi/cbindgen.toml --crate cuinterpose-abi \
		--output ../build/core_abi.h .
	/usr/bin/gcc -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,-Bsymbolic-functions,--version-script=frontend/libcuinterpose.ldscript \
		-I$(CUDA_INCLUDE) -Ibuild frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	rm build/core_abi.h
	chmod 0644 build/libcuinterpose.so

test-native: frontend
	cd rust && cargo fmt --all -- --check
	cd rust && cargo clippy --locked --workspace --all-targets --target x86_64-unknown-linux-gnu -- -D warnings
	cd rust && cargo test --locked --workspace --target x86_64-unknown-linux-gnu
