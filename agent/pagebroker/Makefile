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
MODEL_STREAMER_LIB_DIR ?= /usr/local/lib
MODEL_STREAMER_INCLUDE_DIR ?= /usr/local/include
MODEL_STREAMER_FLAGS := -L$(MODEL_STREAMER_LIB_DIR) -lstreamer -Wl,-rpath,$(MODEL_STREAMER_LIB_DIR)
STREAMER_SOURCES := transfer/engine/model_streamer/model_streamer_restore.cpp
RESTORE_SOURCES := file_descriptor.cpp $(STREAMER_SOURCES)
ENGINE_SOURCES = $(COMMON_SOURCES) $(STREAMER_SOURCES) transfer/engine/model_streamer/model_streamer_transfer_engine.cpp transfer/engine/transfer_engine.cpp
TRANSFER_TEST_SOURCES := tests/transaction_test.cpp transfer/engine/model_streamer/tests/model_streamer_restore_test.cpp tests/temporary_directory_test.cpp
MODEL_STREAMER_RECOVERY_TEST_SOURCES = $(ENGINE_SOURCES) transfer/engine/model_streamer/tests/model_streamer_transfer_engine_test.cpp
MODEL_STREAMER_SESSION_TEST_SOURCES := $(RESTORE_SOURCES) transfer/engine/model_streamer/tests/model_streamer_session_test.cpp
# Inject filesystem failures and cancellation deterministically in the session tests.
# The clock wrapper lets validation observe cancellation without timing races.
MODEL_STREAMER_SESSION_TEST_WRAP_FLAGS := -Wl,--wrap=posix_fallocate,--wrap=mmap \
	-Wl,--wrap=_ZNSt6chrono3_V212steady_clock3nowEv \
	-Wl,--wrap=_ZNSt10filesystem16create_directoryERKNS_7__cxx114pathE \
	-Wl,--wrap=_ZNSt10filesystem11permissionsERKNS_7__cxx114pathENS_5permsENS_12perm_optionsE
TRANSFER_FLAGS := $(MODEL_STREAMER_FLAGS)
CPPFLAGS += -I$(MODEL_STREAMER_INCLUDE_DIR)

BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp transfer/engine/posix/posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer/engine/transfer_engine.cpp
FILESYSTEM_SOURCES := transfer/filesystem/filesystem_storage.cpp
BROKER_TEST_SOURCES := tests/broker_transfer_test.cpp tests/broker_checkpoint_test.cpp tests/broker_cleanup_test.cpp tests/broker_transfer_fixture.cpp tests/broker_gpu_test.cpp
# Wrap mkdir and GPU calls to inject failures without requiring CUDA.
BROKER_TEST_WRAP_FLAGS := -Wl,--wrap=mkdir \
	-Wl,--wrap=_ZN8snapshot10pagebroker3gpu9GpuEngine10CheckpointERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE \
	-Wl,--wrap=_ZN8snapshot10pagebroker3gpu9GpuEngine7RestoreERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE
COMMON_SOURCES := file_descriptor.cpp $(FILESYSTEM_SOURCES)
IO_SOURCES := io_engine.cpp
GPU_SOURCES := gpu/engine.cpp gpu/checkpoint.cpp gpu/transfer.cpp gpu/storage_manifest.cpp gpu/cuda_driver.cpp
PAGEBROKER_SOURCES := $(BROKER_SOURCES) $(COMMON_SOURCES) $(IO_SOURCES) $(GPU_SOURCES) $(STREAMER_SOURCES) transfer/engine/model_streamer/model_streamer_transfer_engine.cpp
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
		-Wl,-Bstatic -lprotobuf -Wl,-Bdynamic $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o pagebroker

test: generate build/cancellation-test build/gpu-cuda-driver.o build/gpu-engine-test    | build
	./build/cancellation-test
	./build/gpu-engine-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(PAGEBROKER_SOURCES) daemon.cpp tests/daemon_test.cpp $(BROKER_TEST_SOURCES) tests/filesystem_storage_test.cpp tests/storage_contract_test.cpp $(TRANSFER_TEST_SOURCES) v1/pagebroker.pb.cc $(BROKER_TEST_WRAP_FLAGS) -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o build/pagebroker-test
	./build/pagebroker-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(MODEL_STREAMER_RECOVERY_TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS)  -o build/model-streamer-recovery-test
	./build/model-streamer-recovery-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(MODEL_STREAMER_SESSION_TEST_SOURCES) $(GTEST_FLAGS) $(MODEL_STREAMER_SESSION_TEST_WRAP_FLAGS) -o build/model-streamer-session-test
	./build/model-streamer-session-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/storage_manifest.cpp gpu/storage_manifest_test.cpp -o build/gpu-storage-test
	./build/gpu-storage-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(IO_SOURCES) transfer/engine/transfer_engine.cpp $(COMMON_SOURCES) \
		io_engine_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o build/io-engine-test
	./build/io-engine-test

# Compile the optional driver loader without requiring a CUDA library.
build/cancellation-test: tests/cancellation_test.cpp cancellation.hpp fatal_cleanup.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $< $(GTEST_FLAGS) -o $@

build/gpu-cuda-driver.o: gpu/cuda_driver.cpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -c $< -o $@

# These tests use real files and validate the engine without initializing CUDA.
build/gpu-engine-test: generate transfer/engine/transfer_engine.cpp gpu/engine_test.cpp gpu/engine.cpp gpu/engine.hpp gpu/checkpoint.cpp gpu/checkpoint.hpp gpu/transfer.cpp gpu/transfer.hpp gpu/storage_manifest.cpp gpu/storage_manifest.hpp gpu/cuda_driver.cpp gpu/errors.hpp $(COMMON_SOURCES) $(IO_SOURCES) io_engine.hpp file_descriptor.hpp cancellation.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/engine_test.cpp gpu/engine.cpp gpu/checkpoint.cpp \
		gpu/transfer.cpp gpu/storage_manifest.cpp gpu/cuda_driver.cpp $(COMMON_SOURCES) $(IO_SOURCES) transfer/engine/transfer_engine.cpp v1/pagebroker.pb.cc \
		$(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o $@

# Run manually with the host driver and one visible GPU.
# No checked-in GPU CI job invokes this target.
test-gpu: build/gpu-checkpoint-test build/gpu-transfer-test
	timeout 120 ./build/gpu-checkpoint-test
	timeout 120 ./build/gpu-transfer-test

build/gpu-checkpoint-test: generate $(GPU_TEST_SOURCES) gpu/checkpoint_test.cpp io_engine.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(GPU_TEST_SOURCES) \
		gpu/checkpoint_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) \
		-Wl,--wrap=cuMemcpyHtoDAsync_v2,--wrap=cuCheckpointProcessUnlock -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@

build/gpu-transfer-test: generate transfer/engine/transfer_engine.cpp gpu/transfer.cpp gpu/transfer.hpp gpu/transfer_test.cpp cancellation.hpp $(COMMON_SOURCES) $(IO_SOURCES) file_descriptor.hpp io_engine.hpp | build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) gpu/transfer.cpp gpu/transfer_test.cpp $(COMMON_SOURCES) $(IO_SOURCES) transfer/engine/transfer_engine.cpp v1/pagebroker.pb.cc \
		$(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@
