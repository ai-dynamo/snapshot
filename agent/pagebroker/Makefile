# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROTO := v1/pagebroker.proto
GTEST_FLAGS := $(shell pkg-config --cflags --libs gtest_main)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp allocation_session.cpp allocation_transport.cpp file_descriptor.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp

.PHONY: daemon generate test

CUDA_ROOT ?= /usr/local/cuda
CUDA_LIB_DIR ?= $(CUDA_ROOT)/lib64/stubs
TRANSFER_DIR := ../cmd/cuda-checkpoint-helper

.PHONY: allocation-worker
allocation-worker: generate
	mkdir -p build
	$(CXX) -std=c++20 -Wall -Wextra -Werror -I. -I$(CUDA_ROOT)/include -I$(TRANSFER_DIR) \
		allocation_worker.cpp allocation_transport.cpp file_descriptor.cpp cuda_posix_transfer.cpp \
		$(TRANSFER_DIR)/transfer_config.cpp $(TRANSFER_DIR)/content_digest.cpp v1/pagebroker.pb.cc \
		-L$(CUDA_LIB_DIR) -lcuda -lprotobuf -lcrypto -pthread -o build/pagebroker-allocation-worker

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc -lprotobuf -o pagebroker

test: generate $(BROKER_SOURCES) daemon_test.cpp
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) daemon_test.cpp v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test

.PHONY: test-allocations
test-allocations: daemon
	mkdir -p build
	protoc --proto_path=. --python_out=build $(PROTO)
	python3 allocation_session_test.py

.PHONY: test-allocation-transfer
test-allocation-transfer: generate
	mkdir -p build
	$(CXX) -std=c++20 -Wall -Wextra -Werror -I. -I$(CUDA_ROOT)/include -I$(TRANSFER_DIR) \
		cuda_posix_transfer_test.cpp cuda_posix_transfer.cpp file_descriptor.cpp \
		$(TRANSFER_DIR)/transfer_config.cpp $(TRANSFER_DIR)/content_digest.cpp \
		-lcrypto $(GTEST_FLAGS) -o build/allocation-transfer-test
	./build/allocation-transfer-test
