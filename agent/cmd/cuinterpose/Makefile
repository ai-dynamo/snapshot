# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build test frontend native test-native

CBINDGEN ?= cbindgen
CUDA_INCLUDE ?= /opt/cuda/include
# The frontend requires GCC 15+ for musttail. Set FRONTEND_CC for local builds.
FRONTEND_CC ?= /opt/gcc-15/bin/gcc

# The preload libraries use GNU/glibc, while the coordinator and launcher use musl so
# they can run in the workload root without depending on its libraries.
build:
	docker buildx build --platform linux/amd64 --target cuinterpose-export \
		--output type=local,dest=build -f ../../Dockerfile ../..

test:
	docker buildx build --platform linux/amd64 --target cuinterpose-test \
		$(if $(TEST_IMAGE),--load --tag "$(TEST_IMAGE)") -f ../../Dockerfile ../..

frontend:
	@cd rust && expected=$$(cargo tree --locked -p cbindgen --depth 0 --prefix none --format '{p}') && \
		actual=$$($(CBINDGEN) --version) && \
		{ test "$$actual" = "cbindgen $${expected#cbindgen v}" || \
		  { echo "ERROR: $$actual does not match locked $$expected" >&2; exit 1; }; }
	mkdir -p build
	# Keep the matching private header in the export for ABI and GPU tests.
	cd rust && $(CBINDGEN) --quiet --config abi/cbindgen.toml --crate cuinterpose-abi \
		--output ../build/core_abi.h .
	$(FRONTEND_CC) -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,--build-id=sha1,-Bsymbolic-functions,--version-script=frontend/libcuinterpose.ldscript \
		-I$(CUDA_INCLUDE) -Ibuild frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	chmod 0644 build/libcuinterpose.so

native: frontend
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-gnu -p cuinterpose-core
	install -m 0644 rust/target/x86_64-unknown-linux-gnu/release/libcuinterpose_core.so build/libcuinterpose_core.so
	sh check-glibc.sh build/libcuinterpose.so build/libcuinterpose_core.so
	# The launcher embeds hashes of these finished libraries, so build it after install.
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-musl -p cuinterpose-coordinator -p cuinterpose-launch
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-coordinator build/cuinterpose-coordinator
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-launch build/cuinterpose-launch

test-native: native
	cd rust && cargo fmt --all -- --check
	cd rust && cargo clippy --locked --workspace --all-targets --target x86_64-unknown-linux-gnu -- -D warnings
	cd rust && cargo test --locked --workspace --target x86_64-unknown-linux-gnu
	cd rust && cargo test --locked --release -p cuinterpose-coordinator -p cuinterpose-protocol -p cuinterpose-launch --target x86_64-unknown-linux-musl
	build/cuinterpose-launch --library "$(CURDIR)/build/libcuinterpose.so" -- /bin/true
