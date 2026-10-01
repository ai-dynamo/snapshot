# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

CXXFLAGS := -O2 -std=c++20 -Wall -Wextra -Werror
CPPFLAGS := -I. -Ibin -I../../pagebroker
CUDA_ROOT ?= /usr/local/cuda
CUDA_LIB_DIR ?= $(CUDA_ROOT)/lib64/stubs
NIXL_ROOT ?=
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
COMMON := ../../pagebroker/file_descriptor.cpp
STORAGE := storage_manifest.cpp extent_digests.cpp content_digest.cpp
TRANSPORT := fd_transport.cpp $(COMMON) bin/helper.pb.cc

# Production requires NIXL. Only isolated transfer tests support a synchronous
# POSIX implementation when NIXL_ROOT is omitted.
ifneq ($(NIXL_ROOT),)
TRANSFER_FLAGS := -DPAGEBROKER_NIXL -I$(NIXL_ROOT)/include -L$(NIXL_ROOT)/lib -lnixl -lnixl_build
endif

.DEFAULT_GOAL := test
.PHONY: test test-storage test-checkpoint test-transfer

test: test-storage test-checkpoint test-transfer

bin:
	mkdir -p bin


test-storage: | bin
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) storage_manifest.cpp storage_manifest_test.cpp -o bin/storage-manifest-test
	./bin/storage-manifest-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) content_digest.cpp content_digest_test.cpp -lcrypto -o bin/content-digest-test
	./bin/content-digest-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(STORAGE) extent_digests_test.cpp $(COMMON) -lcrypto -o bin/extent-digests-test
	./bin/extent-digests-test


test-checkpoint: | bin
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -I$(CUDA_ROOT)/include \
		checkpoint.cpp checkpoint_test.cpp $(GTEST_FLAGS) -pthread -o bin/checkpoint-test
	./bin/checkpoint-test


test-transfer: | bin
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) -I$(CUDA_ROOT)/include \
		transfer_test.cpp transfer.cpp content_digest.cpp $(COMMON) \
		$(GTEST_FLAGS) -lcrypto -pthread $(TRANSFER_FLAGS) -o bin/transfer-test
	./bin/transfer-test
