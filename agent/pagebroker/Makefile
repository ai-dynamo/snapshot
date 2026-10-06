# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

CXXFLAGS := -O2 -std=c++20 -Wall -Wextra -Werror -pthread
CUDA_ROOT ?= /usr/local/cuda
CUDA_INCLUDE_DIR ?= $(CUDA_ROOT)/include
CUDA_LIBRARY_DIR ?= $(CUDA_ROOT)/lib64
NIXL_ROOT ?= /usr/local
NVTX_ROOT ?= /usr/local
CPPFLAGS := -I. -Igpu -I$(CUDA_INCLUDE_DIR) -isystem $(NVTX_ROOT)/include
PROTO := v1/pagebroker.proto
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp
COMMON_SOURCES := file_descriptor.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) $(COMMON_SOURCES) daemon.cpp main.cpp

# Production and transfer tests use the same NIXL storage path.
NIXL_FLAGS := -isystem $(NIXL_ROOT)/include -L$(NIXL_ROOT)/lib -lnixl -lnixl_build

.DEFAULT_GOAL := daemon
.PHONY: daemon generate test test-gpu

generate: v1/pagebroker.pb.cc v1/pagebroker.pb.h

v1/pagebroker.pb.cc v1/pagebroker.pb.h &: $(PROTO)
	protoc --proto_path=. --cpp_out=. $(PROTO)

build:
	mkdir -p build

daemon: generate
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(DAEMON_SOURCES) v1/pagebroker.pb.cc \
		-Wl,-Bstatic -lprotobuf -Wl,-Bdynamic -o pagebroker

test: generate | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(BROKER_SOURCES) $(COMMON_SOURCES) \
		daemon_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/storage_manifest.cpp gpu/storage_manifest_test.cpp -o build/gpu-storage-test
	./build/gpu-storage-test

# GPU CI runs this target with the host driver and one visible GPU.
test-gpu: build/gpu-checkpoint-test build/gpu-transfer-test
	timeout 120 ./build/gpu-checkpoint-test
	timeout 120 ./build/gpu-transfer-test

build/gpu-checkpoint-test: gpu/checkpoint.cpp gpu/checkpoint.hpp gpu/checkpoint_test.cpp file_descriptor.cpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/checkpoint.cpp gpu/checkpoint_test.cpp file_descriptor.cpp \
		$(GTEST_FLAGS) -L$(CUDA_LIBRARY_DIR) -lcuda -o $@

build/gpu-transfer-test: gpu/transfer.cpp gpu/transfer.hpp gpu/transfer_test.cpp cancellation.hpp gpu/errors.hpp file_descriptor.cpp file_descriptor.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/transfer.cpp gpu/transfer_test.cpp file_descriptor.cpp \
		$(GTEST_FLAGS) $(NIXL_FLAGS) -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@
