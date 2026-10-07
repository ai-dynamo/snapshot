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
IO_SOURCES := io_engine.cpp
GPU_SOURCES := gpu/engine.cpp gpu/checkpoint.cpp gpu/transfer.cpp gpu/storage_manifest.cpp gpu/cuda_driver.cpp
PAGEBROKER_SOURCES := $(BROKER_SOURCES) $(COMMON_SOURCES) $(IO_SOURCES) $(GPU_SOURCES)
DAEMON_SOURCES := $(PAGEBROKER_SOURCES) daemon.cpp main.cpp
# Real-driver tests resolve CUDA symbols from libcuda instead of the optional loader.
GPU_TEST_SOURCES := $(filter-out gpu/cuda_driver.cpp,$(PAGEBROKER_SOURCES))

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
	@test -f "$(NIXL_ROOT)/include/nixl.h" || { echo 'NIXL header not found: $(NIXL_ROOT)/include/nixl.h'; exit 1; }
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(DAEMON_SOURCES) v1/pagebroker.pb.cc \
		-Wl,-Bstatic -lprotobuf -Wl,-Bdynamic $(NIXL_FLAGS) -ldl -o pagebroker

test: generate build/gpu-cuda-driver.o build/gpu-engine-test | build
	./build/gpu-engine-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(PAGEBROKER_SOURCES) daemon.cpp daemon_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) -ldl -o build/pagebroker-test
	./build/pagebroker-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/storage_manifest.cpp gpu/storage_manifest_test.cpp -o build/gpu-storage-test
	./build/gpu-storage-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(IO_SOURCES) transfer_engine.cpp file_descriptor.cpp \
		io_engine_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) -ldl -o build/io-engine-test
	./build/io-engine-test

# Compile the optional driver loader without requiring a CUDA library.
build/gpu-cuda-driver.o: gpu/cuda_driver.cpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -c $< -o $@

# These tests use real files and validate the engine without initializing CUDA.
build/gpu-engine-test: generate transfer_engine.cpp gpu/engine_test.cpp gpu/engine.cpp gpu/engine.hpp gpu/checkpoint.cpp gpu/checkpoint.hpp gpu/transfer.cpp gpu/transfer.hpp gpu/storage_manifest.cpp gpu/storage_manifest.hpp gpu/cuda_driver.cpp gpu/errors.hpp $(COMMON_SOURCES) $(IO_SOURCES) io_engine.hpp file_descriptor.hpp cancellation.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/engine_test.cpp gpu/engine.cpp gpu/checkpoint.cpp \
		gpu/transfer.cpp gpu/storage_manifest.cpp gpu/cuda_driver.cpp $(COMMON_SOURCES) $(IO_SOURCES) transfer_engine.cpp v1/pagebroker.pb.cc \
		$(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) -ldl -o $@

# Requires the host driver and one visible GPU. CI runs the compiled binaries
# from the GPU test image without rebuilding them on the GPU node.
test-gpu: build/gpu-checkpoint-test build/gpu-transfer-test
	timeout 120 ./build/gpu-checkpoint-test
	timeout 120 ./build/gpu-transfer-test

build/gpu-checkpoint-test: generate $(GPU_TEST_SOURCES) gpu/checkpoint_test.cpp io_engine.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(GPU_TEST_SOURCES) \
		gpu/checkpoint_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) \
		-Wl,--wrap=cuMemcpyHtoDAsync_v2,--wrap=cuCheckpointProcessUnlock -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@

build/gpu-transfer-test: generate transfer_engine.cpp gpu/transfer.cpp gpu/transfer.hpp gpu/transfer_test.cpp cancellation.hpp $(COMMON_SOURCES) $(IO_SOURCES) file_descriptor.hpp io_engine.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/transfer.cpp gpu/transfer_test.cpp $(COMMON_SOURCES) $(IO_SOURCES) transfer_engine.cpp v1/pagebroker.pb.cc \
		$(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@
