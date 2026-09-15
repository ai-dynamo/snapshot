# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := build
.PHONY: build test frontend native test-native
CXXFLAGS := -std=c++20 -O2 -g -Wall -Wextra -Werror -pthread
CORE := protocol state process control export_cache host_carrier multicast
OBJECTS := $(addprefix build/,$(addsuffix .o,$(CORE)))

# The coordinator is fully static because it enters an arbitrary workload root.
# The backend shares the workload's normal GNU C++ runtime.
build:
	docker buildx build --platform linux/amd64 --target cuinterpose-export \
		--output type=local,dest=build -f ../../Dockerfile ../..

test:
	docker buildx build --platform linux/amd64 --target cuinterpose-test \
		-f ../../Dockerfile ../..

frontend:
	mkdir -p build
	/usr/bin/gcc -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,-Bsymbolic-functions -I. frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	chmod 0644 build/libcuinterpose.so

build/%.o: cpp/%.cpp cpp/protocol.hpp cpp/state.hpp cpp/driver.hpp core_abi.h
	mkdir -p build
	$(CXX) $(CXXFLAGS) -fPIC -fvisibility=hidden -c $< -o $@

build/libcuinterpose_core.so: $(OBJECTS) cpp/exports.map
	$(CXX) $(CXXFLAGS) -shared $(OBJECTS) -Wl,--version-script=cpp/exports.map -o $@
	chmod 0644 $@

build/cuinterpose-coordinator: build/coordinator.o build/protocol.o
	$(CXX) $(CXXFLAGS) -static build/coordinator.o build/protocol.o -o $@

native: frontend build/libcuinterpose_core.so build/cuinterpose-coordinator
	python3 verify_artifacts.py build

test-native: native
	$(CXX) $(CXXFLAGS) cpp/tests/protocol.cpp build/protocol.o build/export_cache.o -o build/protocol-test
	build/protocol-test
	$(CXX) $(CXXFLAGS) cpp/tests/state.cpp $(OBJECTS) -o build/state-test
	build/state-test
	python3 cpp/tests/coordinator.py build/cuinterpose-coordinator
	python3 tests/headless/headless.py --artifacts build
