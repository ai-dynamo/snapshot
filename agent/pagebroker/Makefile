# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

CXXFLAGS := -O2 -std=c++20 -Wall -Wextra -Werror
CPPFLAGS := -I.
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
CUDA_ROOT ?= /usr/local/cuda
CUDA_LIB_DIR ?= $(CUDA_ROOT)/lib64/stubs
NIXL_ROOT ?=

COMMON_SOURCES := file_descriptor.cpp
PROTOCOL_SOURCES := v1/pagebroker.pb.cc
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp $(COMMON_SOURCES) $(PROTOCOL_SOURCES)
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp
GPU_MANIFEST_SOURCES := gpu_engine/storage_manifest.cpp
GPU_INTEGRITY_SOURCES := gpu_engine/integrity/extent_digests.cpp
HASH_SOURCES := integrity/content_digest.cpp
CUDA_CHECKPOINT_SOURCES := gpu_engine/cuda_checkpoint/checkpoint.cpp gpu_engine/cuda_checkpoint/driver_ops.c

.PHONY: daemon generate test test-storage test-integrity test-cuda-helper

generate:
	protoc --proto_path=. --cpp_out=. v1/pagebroker.proto

daemon: generate
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(DAEMON_SOURCES) -Wl,-Bstatic -lprotobuf -Wl,-Bdynamic -pthread -o pagebroker

test: generate
	mkdir -p build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(BROKER_SOURCES) daemon_test.cpp -lprotobuf $(GTEST_FLAGS) -pthread -o build/pagebroker-test
	./build/pagebroker-test

test-storage: test-integrity
	mkdir -p build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(GPU_MANIFEST_SOURCES) gpu_engine/storage_manifest_test.cpp -o build/storage-manifest-test
	./build/storage-manifest-test

test-integrity:
	mkdir -p build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) integrity/content_digest.cpp integrity/content_digest_test.cpp -lcrypto -o build/content-digest-test
	./build/content-digest-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) integrity/content_digest.cpp \
		gpu_engine/integrity/extent_digests.cpp gpu_engine/integrity/extent_digests_test.cpp $(GPU_MANIFEST_SOURCES) $(COMMON_SOURCES) -lcrypto -o build/extent-digests-test
	./build/extent-digests-test

test-cuda-helper:
	mkdir -p build
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -I$(CUDA_ROOT)/include \
		$(CUDA_CHECKPOINT_SOURCES) gpu_engine/cuda_checkpoint/checkpoint_test.cpp \
		$(GTEST_FLAGS) -pthread -o build/cuda-helper-test
	./build/cuda-helper-test
