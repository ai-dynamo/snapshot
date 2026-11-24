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
STREAMER_SOURCES := model_streamer_config.cpp transfer/engine/model_streamer/model_streamer_restore.cpp
S3_SOURCES := s3_config.cpp transfer/s3/s3_client.cpp transfer/s3/s3_storage_backend.cpp transfer/s3/checkpoint_index.cpp
RESTORE_SOURCES := file_descriptor.cpp $(STREAMER_SOURCES)
ENGINE_SOURCES = $(COMMON_SOURCES) $(STREAMER_SOURCES) $(S3_SOURCES) transfer/engine/model_streamer/model_streamer_transfer_engine.cpp transfer/engine/transfer_engine.cpp
TRANSFER_TEST_SOURCES := tests/model_streamer_config_test.cpp tests/daemon_options_test.cpp tests/transaction_test.cpp transfer/s3/tests/checkpoint_index_test.cpp transfer/s3/tests/s3_storage_backend_test.cpp tests/s3_config_test.cpp transfer/engine/model_streamer/tests/model_streamer_restore_test.cpp transfer/s3/tests/s3_client_test.cpp tests/temporary_directory_test.cpp
MODEL_STREAMER_RECOVERY_TEST_SOURCES = $(ENGINE_SOURCES) transfer/engine/model_streamer/tests/model_streamer_transfer_engine_test.cpp
MODEL_STREAMER_SESSION_TEST_SOURCES := $(RESTORE_SOURCES) transfer/engine/model_streamer/tests/model_streamer_session_test.cpp
# Inject filesystem/queue failures and hold producer progress in the session tests.
MODEL_STREAMER_SESSION_TEST_WRAP_FLAGS := -Wl,--wrap=posix_fallocate,--wrap=mmap,--wrap=_Znwm \
	-Wl,--wrap=_ZNSt6chrono3_V212steady_clock3nowEv \
	-Wl,--wrap=_ZNSt10filesystem16create_directoryERKNS_7__cxx114pathE \
	-Wl,--wrap=_ZNSt10filesystem11permissionsERKNS_7__cxx114pathENS_5permsENS_12perm_optionsE
TRANSFER_FLAGS = $(MODEL_STREAMER_FLAGS)
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
GPU_SOURCES := gpu/engine.cpp gpu/checkpoint.cpp transfer/engine/posix/gpu_transfer.cpp gpu/storage_manifest.cpp gpu/cuda_driver.cpp
PAGEBROKER_SOURCES := $(BROKER_SOURCES) $(COMMON_SOURCES) $(IO_SOURCES) $(GPU_SOURCES) $(STREAMER_SOURCES) $(S3_SOURCES) transfer/engine/model_streamer/model_streamer_transfer_engine.cpp
DAEMON_SOURCES := $(PAGEBROKER_SOURCES) daemon.cpp main.cpp
# Real-driver tests resolve CUDA symbols from libcuda instead of the optional loader.
GPU_TEST_SOURCES := $(filter-out gpu/cuda_driver.cpp,$(PAGEBROKER_SOURCES))

# Production and transfer tests use the same NIXL storage path.
NIXL_FLAGS := -isystem $(NIXL_ROOT)/include -L$(NIXL_ROOT)/lib -lnixl -lnixl_build

# Compile each compatible translation unit once, then retain each binary's link flags.
objects = $(addprefix build/,$(addsuffix .o,$(basename $(1))))
PROTO_OBJECT := build/v1/pagebroker.pb.o
PAGEBROKER_TEST_SOURCES := $(PAGEBROKER_SOURCES) daemon.cpp tests/daemon_test.cpp $(BROKER_TEST_SOURCES) tests/filesystem_storage_test.cpp tests/storage_contract_test.cpp $(TRANSFER_TEST_SOURCES)
GPU_ENGINE_TEST_SOURCES := $(GPU_SOURCES) $(COMMON_SOURCES) $(IO_SOURCES) transfer/engine/transfer_engine.cpp gpu/engine_test.cpp
IO_ENGINE_TEST_SOURCES := $(IO_SOURCES) transfer/engine/transfer_engine.cpp $(COMMON_SOURCES) io_engine_test.cpp
GPU_TRANSFER_TEST_SOURCES := transfer/engine/transfer_engine.cpp transfer/engine/posix/gpu_transfer.cpp transfer/engine/posix/tests/gpu_transfer_test.cpp $(COMMON_SOURCES) $(IO_SOURCES)
S3_INVENTORY_TEST_SOURCES := $(ENGINE_SOURCES) gpu/storage_manifest.cpp transfer/s3/tests/s3_restore_inventory_test.cpp
RCLONE_TEST_SOURCES := transfer/s3/s3_client.cpp file_descriptor.cpp transfer/s3/tests/rclone_process_test.cpp
RCLONE_TEST_OBJECTS := $(patsubst build/%,build/rclone/%,$(call objects,$(RCLONE_TEST_SOURCES)))
GTEST_SOURCES := $(BROKER_TEST_SOURCES) $(TRANSFER_TEST_SOURCES) tests/daemon_test.cpp tests/filesystem_storage_test.cpp tests/storage_contract_test.cpp \
    transfer/engine/model_streamer/tests/model_streamer_transfer_engine_test.cpp transfer/engine/model_streamer/tests/model_streamer_session_test.cpp \
    tests/cancellation_test.cpp gpu/engine_test.cpp io_engine_test.cpp gpu/checkpoint_test.cpp transfer/engine/posix/tests/gpu_transfer_test.cpp transfer/s3/tests/s3_restore_inventory_test.cpp
OBJECTS := $(sort $(call objects,$(DAEMON_SOURCES) $(PAGEBROKER_TEST_SOURCES) $(GPU_ENGINE_TEST_SOURCES) $(IO_ENGINE_TEST_SOURCES) $(GPU_TRANSFER_TEST_SOURCES) $(S3_INVENTORY_TEST_SOURCES) $(MODEL_STREAMER_RECOVERY_TEST_SOURCES) $(MODEL_STREAMER_SESSION_TEST_SOURCES) $(GTEST_SOURCES) gpu/storage_manifest_test.cpp) \
    $(PROTO_OBJECT) $(RCLONE_TEST_OBJECTS))
TEST_BINARIES := build/s3-restore-inventory-test build/rclone-process-test build/cancellation-test build/gpu-engine-test \
    build/pagebroker-test build/model-streamer-recovery-test build/model-streamer-session-test build/gpu-storage-test build/io-engine-test

.DEFAULT_GOAL := daemon
.DELETE_ON_ERROR:
.PHONY: daemon generate test test-gpu

generate: v1/pagebroker.pb.cc v1/pagebroker.pb.h

v1/pagebroker.pb.cc v1/pagebroker.pb.h &: $(PROTO)
	protoc --proto_path=. --cpp_out=. $(PROTO)

# Makefile edits rebuild objects. Use make -B when changing command-line flags
# or compiler installations in an existing build directory.
$(OBJECTS): Makefile
$(call objects,$(GTEST_SOURCES)): CPPFLAGS += $(shell pkg-config --cflags gtest_main)

build/%.o: %.cpp | v1/pagebroker.pb.h
	mkdir -p $(@D)
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -isystem $(NIXL_ROOT)/include -MMD -MP -c $< -o $@

build/%.o: %.cc | v1/pagebroker.pb.h
	mkdir -p $(@D)
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -isystem $(NIXL_ROOT)/include -MMD -MP -c $< -o $@

# This test injects its child executable path into both the client and the test.
build/rclone/%.o: %.cpp | v1/pagebroker.pb.h
	mkdir -p $(@D)
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(shell pkg-config --cflags gtest_main) \
		-DPAGEBROKER_RCLONE_PATH='"$(abspath build/rclone-child-helper)"' -MMD -MP -c $< -o $@

-include $(OBJECTS:.o=.d)

daemon: pagebroker

pagebroker: $(call objects,$(DAEMON_SOURCES)) $(PROTO_OBJECT)
	@test -f "$(NIXL_ROOT)/include/nixl.h" || { echo 'NIXL header not found: $(NIXL_ROOT)/include/nixl.h'; exit 1; }
	$(CXX) $(CXXFLAGS) $^ -Wl,-Bstatic -lprotobuf -Wl,-Bdynamic $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o $@

test: $(TEST_BINARIES)
	./build/s3-restore-inventory-test
	./build/rclone-process-test
	./build/cancellation-test
	./build/gpu-engine-test
	./build/pagebroker-test
	./build/model-streamer-recovery-test
	./build/model-streamer-session-test
	./build/gpu-storage-test
	./build/io-engine-test

build/pagebroker-test: $(call objects,$(PAGEBROKER_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ $(BROKER_TEST_WRAP_FLAGS) -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o $@

build/model-streamer-recovery-test: $(call objects,$(MODEL_STREAMER_RECOVERY_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ -lprotobuf $(GTEST_FLAGS) -o $@

build/model-streamer-session-test: $(call objects,$(MODEL_STREAMER_SESSION_TEST_SOURCES))
	$(CXX) $(CXXFLAGS) $^ $(GTEST_FLAGS) $(MODEL_STREAMER_SESSION_TEST_WRAP_FLAGS) -o $@

build/gpu-storage-test: build/gpu/storage_manifest.o build/gpu/storage_manifest_test.o
	$(CXX) $(CXXFLAGS) $^ -o $@

build/io-engine-test: $(call objects,$(IO_ENGINE_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o $@

build/cancellation-test: build/tests/cancellation_test.o
	$(CXX) $(CXXFLAGS) $^ $(GTEST_FLAGS) -o $@

# These tests use real files and validate the engine without initializing CUDA.
build/gpu-engine-test: $(call objects,$(GPU_ENGINE_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ $(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o $@

# Run manually with the host driver and one visible GPU.
# No checked-in GPU CI job invokes this target.
test-gpu: build/gpu-checkpoint-test build/gpu-transfer-test
	timeout 120 ./build/gpu-checkpoint-test
	timeout 120 ./build/gpu-transfer-test

build/gpu-checkpoint-test: $(call objects,$(GPU_TEST_SOURCES) gpu/checkpoint_test.cpp) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) \
		-Wl,--wrap=cuMemcpyHtoDAsync_v2,--wrap=cuCheckpointProcessUnlock,--wrap=cuStreamCreate,--wrap=cuMemCreate,--wrap=cuMemSetAccess -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@

build/gpu-transfer-test: $(call objects,$(GPU_TRANSFER_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ $(GTEST_FLAGS) -lprotobuf $(NIXL_FLAGS) -L$(CUDA_LIBRARY_DIR) -lcuda -ldl -o $@

# Test fork/exec with a compiled child, never a shell script or real credentials.
build/rclone-child-helper: transfer/s3/tests/rclone_child_helper.cpp Makefile
	mkdir -p $(@D)
	$(CXX) $(CXXFLAGS) $< -o $@

build/rclone-process-test: $(RCLONE_TEST_OBJECTS) | build/rclone-child-helper
	$(CXX) $(CXXFLAGS) $^ $(GTEST_FLAGS) \
		-Wl,--wrap=pthread_cond_clockwait,--wrap=_ZNSt6chrono3_V212steady_clock3nowEv -o $@

rclone-process-test: build/rclone-process-test
	./build/rclone-process-test

.PHONY: rclone-process-test

# Intercept only metadata GET; payload probes must not be part of preparation.
build/s3-restore-inventory-test: $(call objects,$(S3_INVENTORY_TEST_SOURCES)) $(PROTO_OBJECT)
	$(CXX) $(CXXFLAGS) $^ -Wl,--wrap=_ZN8snapshot10pagebroker8S3Client3GetB5cxx11ERKNS0_16S3ObjectLocationEmNS0_12CancellationE \
		-lprotobuf $(GTEST_FLAGS) $(TRANSFER_FLAGS) -o $@

s3-restore-inventory-test: build/s3-restore-inventory-test
	./build/s3-restore-inventory-test

.PHONY: s3-restore-inventory-test
