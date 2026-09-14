# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROTO := v1/pagebroker.proto
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
GTEST_NO_MAIN_FLAGS = $(shell pkg-config --cflags --libs gtest)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp daemon_connection.cpp file_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp stage_ready_gate.cpp transaction.cpp transfer_engine.cpp
CUDA_CORE_DIR := ../cmd/cuda-checkpoint-helper
CUINTERPOSE_DIR := ../cmd/cuinterpose
CUINTERPOSE_TEST_BINARY := $(abspath build/cuinterpose/cuinterpose-coordinator)
CUDA_SOURCES := cuda_engine.cpp cuda_posix_transfer.cpp cuda_worker_client.cpp \
	cuda_worker_pool.cpp cuinterpose_coordinator.cpp \
	$(CUDA_CORE_DIR)/content_digest.cpp \
	$(CUDA_CORE_DIR)/cuda_operation.cpp \
	$(CUDA_CORE_DIR)/daemon_protocol.cpp \
	$(CUDA_CORE_DIR)/storage_manifest.cpp \
	$(CUDA_CORE_DIR)/transfer_config.cpp \
	$(CUDA_CORE_DIR)/transfer_scheduler.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp $(CUDA_SOURCES)
CUDA_WORKER_SOURCES := cuda_worker_main.cpp cuda_posix_transfer.cpp \
	$(CUDA_CORE_DIR)/content_digest.cpp \
	$(CUDA_CORE_DIR)/cuda_operation.cpp \
	$(CUDA_CORE_DIR)/daemon_protocol.cpp \
	$(CUDA_CORE_DIR)/daemon_server.cpp \
	$(CUDA_CORE_DIR)/storage_manifest.cpp \
	$(CUDA_CORE_DIR)/transfer_config.cpp \
	$(CUDA_CORE_DIR)/transfer_scheduler.cpp

.PHONY: daemon generate pagebroker-cuda-worker stage-ready-gate-test test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

pagebroker-cuda-worker:
	c++ -I. -I$(CUDA_CORE_DIR) -I/opt/cuda-13.4/include -std=c++20 -Wall -Werror -pthread \
		$(CUDA_WORKER_SOURCES) -L/usr/local/cuda/lib64/stubs -lcuda -lcrypto \
		-o pagebroker-cuda-worker

daemon: generate pagebroker-cuda-worker
	c++ -I. -I$(CUDA_CORE_DIR) -I$(CUINTERPOSE_DIR) -I/opt/cuda-13.4/include -std=c++20 -Wall -Werror -pthread \
		$(DAEMON_SOURCES) v1/pagebroker.pb.cc -L/usr/local/cuda/lib64/stubs \
		-lprotobuf -lcuda -lcrypto -o pagebroker

stage-ready-gate-test: stage_ready_gate.cpp stage_ready_gate.hpp stage_ready_gate_test.cpp file_descriptor.cpp
	mkdir -p build
	c++ -I. -I$(CUDA_CORE_DIR) -std=c++20 -Wall -Werror -pthread \
		stage_ready_gate.cpp file_descriptor.cpp stage_ready_gate_test.cpp \
		$(CUDA_CORE_DIR)/content_digest.cpp -lprotobuf -lcrypto $(GTEST_FLAGS) \
		-o build/stage-ready-gate-test
	./build/stage-ready-gate-test

test: generate $(BROKER_SOURCES) daemon_test.cpp daemon_connection_test.cpp cuda_engine_test.cpp cuinterpose_coordinator.cpp cuinterpose_coordinator_test.cpp posix_copy_engine_test.cpp stage_ready_gate_test.cpp transaction_test.cpp
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror transaction.cpp \
		checkpoint_transaction_descriptor.cpp restore_transaction_descriptor.cpp \
		file_descriptor.cpp transaction_test.cpp v1/pagebroker.pb.cc -lprotobuf \
		$(GTEST_FLAGS) -o build/transaction-test
	./build/transaction-test
	$(MAKE) -C $(CUINTERPOSE_DIR) BUILD_DIR=$(abspath build/cuinterpose) $(CUINTERPOSE_TEST_BINARY)
	c++ -I. -I$(CUDA_CORE_DIR) -I$(CUINTERPOSE_DIR) -I/opt/cuda-13.4/include \
		-std=c++20 -Wall -Werror -pthread $(BROKER_SOURCES) daemon.cpp $(CUDA_SOURCES) daemon_test.cpp \
		v1/pagebroker.pb.cc -L/usr/local/cuda/lib64/stubs \
		-lprotobuf -lcuda -lcrypto $(GTEST_FLAGS) -o build/pagebroker-test
	LD_LIBRARY_PATH=/usr/local/cuda/lib64/stubs ./build/pagebroker-test
	c++ -I. -I$(CUDA_CORE_DIR) -std=c++20 -Wall -Werror -pthread posix_copy_engine.cpp file_descriptor.cpp \
		transfer_engine.cpp posix_copy_engine_test.cpp \
		$(CUDA_CORE_DIR)/content_digest.cpp $(CUDA_CORE_DIR)/storage_manifest.cpp \
		v1/pagebroker.pb.cc -lprotobuf -lcrypto $(GTEST_FLAGS) -o build/posix-copy-engine-test
	./build/posix-copy-engine-test
	$(MAKE) stage-ready-gate-test
	c++ -I. -std=c++20 -Wall -Werror daemon_connection.cpp daemon_connection_test.cpp \
		v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/daemon-connection-test
	./build/daemon-connection-test
	c++ -I. -I$(CUDA_CORE_DIR) -I$(CUINTERPOSE_DIR) -std=c++20 -Wall -Werror \
		cuinterpose_coordinator.cpp cuinterpose_coordinator_test.cpp \
		$(CUDA_CORE_DIR)/content_digest.cpp -lcrypto $(GTEST_FLAGS) \
		-o build/cuinterpose-coordinator-test
	./build/cuinterpose-coordinator-test
	c++ -I. -I$(CUDA_CORE_DIR) -I$(CUINTERPOSE_DIR) -I/opt/cuda-13.4/include -std=c++20 -Wall -Werror -pthread \
		file_descriptor.cpp $(CUDA_SOURCES) cuda_engine_test.cpp v1/pagebroker.pb.cc \
		-L/usr/local/cuda/lib64/stubs -lprotobuf -lcuda -lcrypto $(GTEST_FLAGS) \
		-o build/cuda-engine-test
	ln -sf /usr/local/cuda/lib64/stubs/libcuda.so build/libcuda.so.1
	LD_LIBRARY_PATH=$(abspath build) ./build/cuda-engine-test
	c++ -I. -I$(CUDA_CORE_DIR) -I/opt/cuda-13.4/include -std=c++20 -Wall -Werror -pthread \
		cuda_worker_client.cpp cuda_posix_transfer.cpp cuda_worker_descriptor_test.cpp \
		$(CUDA_CORE_DIR)/content_digest.cpp $(CUDA_CORE_DIR)/cuda_operation.cpp \
		$(CUDA_CORE_DIR)/daemon_protocol.cpp $(CUDA_CORE_DIR)/daemon_server.cpp \
		$(CUDA_CORE_DIR)/storage_manifest.cpp $(CUDA_CORE_DIR)/transfer_config.cpp \
		$(CUDA_CORE_DIR)/transfer_scheduler.cpp -L/usr/local/cuda/lib64/stubs \
		-lcuda -lcrypto $(GTEST_FLAGS) -o build/cuda-worker-descriptor-test
	LD_LIBRARY_PATH=$(abspath build) ./build/cuda-worker-descriptor-test
	c++ -I. -I$(CUDA_CORE_DIR) -std=c++20 -Wall -Werror -pthread \
		cuda_worker_fake.cpp $(CUDA_CORE_DIR)/daemon_protocol.cpp \
		-o build/cuda-worker-fake
	c++ -I. -I$(CUDA_CORE_DIR) -std=c++20 -Wall -Werror -pthread \
		cuda_worker_client.cpp cuda_worker_pool.cpp cuda_worker_pool_test.cpp \
		$(CUDA_CORE_DIR)/daemon_protocol.cpp $(GTEST_NO_MAIN_FLAGS) \
		-o build/cuda-worker-pool-test
	./build/cuda-worker-pool-test ./build/cuda-worker-fake
