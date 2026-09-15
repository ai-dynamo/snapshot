# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := core
.PHONY: build test frontend native test-native
CXXFLAGS := -std=c++20 -O2 -g -Wall -Wextra -Werror -pthread
CORE := protocol state process control export_cache host_carrier multicast
OBJECTS := $(addprefix build/,$(addsuffix .o,$(CORE)))

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

core: frontend build/libcuinterpose_core.so
