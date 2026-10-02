# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build test frontend native test-native test-gpu

CBINDGEN ?= cbindgen
CUDA_INCLUDE ?= /opt/cuda/include
# The frontend requires GCC 15+ for musttail. Set FRONTEND_CC for local builds.
FRONTEND_CC ?= /opt/gcc-15/bin/gcc
UV ?= uv
GPU_BUILD_DIR ?= $(CURDIR)/build

# Keep the preload libraries on GNU/glibc. The coordinator and launcher use
# musl so they can execute in an arbitrary workload root.
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
	$(FRONTEND_CC) -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,--build-id=sha1,-Bsymbolic-functions,--version-script=frontend/libcuinterpose.ldscript \
		-I$(CUDA_INCLUDE) -Ibuild frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	chmod 0644 build/libcuinterpose.so

native: frontend
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-gnu -p cuinterpose-core
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-musl -p cuinterpose-coordinator -p cuinterpose-launch
	install -m 0644 rust/target/x86_64-unknown-linux-gnu/release/libcuinterpose_core.so build/libcuinterpose_core.so
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-coordinator build/cuinterpose-coordinator
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-launch build/cuinterpose-launch

# CPU checks cover Rust bookkeeping, protocols, and coordinator sockets.
test-native: native
	cd rust && cargo fmt --all -- --check
	cd rust && cargo clippy --locked --workspace --all-targets --target x86_64-unknown-linux-gnu -- -D warnings
	cd rust && cargo test --locked --workspace --target x86_64-unknown-linux-gnu
	python3 tests/coordinator.py
	cd rust && cargo test --locked --release -p cuinterpose-coordinator -p cuinterpose-protocol -p cuinterpose-launch --target x86_64-unknown-linux-musl

# Build/export matching artifacts first; this host needs two real CUDA GPUs.
test-gpu:
	python3 frontend/tests/run.py --artifacts "$(GPU_BUILD_DIR)" --cuda-include "$(CUDA_INCLUDE)"
	CUINTERPOSE_BUILD_DIR="$(GPU_BUILD_DIR)" $(UV) run --project tests/gpu pytest tests/gpu -v
