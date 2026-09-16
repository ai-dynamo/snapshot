# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build frontend native

# Keep the preload libraries on GNU/glibc. Only the standalone coordinator
# uses musl, because it executes after entering an arbitrary workload root.
build:
	docker buildx build --platform linux/amd64 --target cuinterpose-export \
		--output type=local,dest=build -f ../../Dockerfile ../..

frontend:
	mkdir -p build
	cd rust && cargo run --locked --quiet --example bindings -p cuinterpose-abi
	/usr/bin/gcc -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,-Bsymbolic-functions -Ibuild frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	rm build/core_abi.h
	chmod 0644 build/libcuinterpose.so

native: frontend
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-gnu -p cuinterpose-core
	cd rust && cargo build --locked --release --target x86_64-unknown-linux-musl -p cuinterpose-coordinator
	install -m 0644 rust/target/x86_64-unknown-linux-gnu/release/libcuinterpose_core.so build/libcuinterpose_core.so
	install -m 0755 rust/target/x86_64-unknown-linux-musl/release/cuinterpose-coordinator build/cuinterpose-coordinator
	python3 verify_artifacts.py build
