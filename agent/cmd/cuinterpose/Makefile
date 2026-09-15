# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build test native test-native

# Keep the preload libraries on GNU/glibc. Only the standalone coordinator
# uses musl, because it executes after entering an arbitrary workload root.
build:
	docker buildx build --platform linux/amd64 --target cuinterpose-export \
		--output type=local,dest=build -f ../../Dockerfile ../..

test:
	docker buildx build --platform linux/amd64 --target cuinterpose-test \
		-f ../../Dockerfile ../..

native:
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-gnu -p cuinterpose -p cuinterpose-core
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-musl -p cuinterpose-coordinator
	install -D -m 0644 rust/target/x86_64-unknown-linux-gnu/release/libcuinterpose.so build/libcuinterpose.so
	install -m 0644 rust/target/x86_64-unknown-linux-gnu/release/libcuinterpose_core.so build/libcuinterpose_core.so
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-coordinator build/cuinterpose-coordinator
	python3 verify_artifacts.py build

test-native: native
	cd rust && cargo fmt --all -- --check
	cd rust && cargo test --locked --workspace --target x86_64-unknown-linux-gnu
	cd rust && python3 frontend/tests/run.py
	cd rust && cargo test --locked --release -p cuinterpose-coordinator -p cuinterpose-protocol --target x86_64-unknown-linux-musl
