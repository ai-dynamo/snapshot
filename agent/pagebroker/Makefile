# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROTO := v1/pagebroker.proto
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp file_descriptor.cpp native_session.cpp gpu_engine.cpp fd_transport.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp

.PHONY: daemon generate test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO) gpu_engine.proto

daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc gpu_engine.pb.cc -lprotobuf -o pagebroker

test: test-gpu-engine generate $(BROKER_SOURCES) daemon_test.cpp
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) daemon_test.cpp v1/pagebroker.pb.cc gpu_engine.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test

.PHONY: test-storage
test-storage: test-integrity test-transfer-contracts
	mkdir -p build
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror storage_manifest.cpp storage_manifest_test.cpp -o build/storage-manifest-test
	./build/storage-manifest-test

# Optional modules are tested here but are not linked into the production data
# plane. Keeping them available adds no hashing or layout planning to transfers.
.PHONY: test-integrity test-transfer-contracts check-transfer-contracts-cuda
test-integrity:
	mkdir -p build
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror integrity/content_digest.cpp integrity/content_digest_test.cpp -lcrypto -o build/content-digest-test
	./build/content-digest-test
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror integrity/content_digest.cpp integrity/extent_digests.cpp integrity/extent_digests_test.cpp -lcrypto -o build/extent-digests-test
	./build/extent-digests-test

test-transfer-contracts:
	mkdir -p build
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror transfer_contracts/transfer_config.cpp transfer_contracts/transfer_config_test.cpp -o build/transfer-config-test
	./build/transfer-config-test
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror transfer_contracts/transfer_engine_test.cpp -o build/transfer-cancellation-test
	./build/transfer-cancellation-test

CUDA_ROOT ?= /usr/local/cuda
check-transfer-contracts-cuda:
	mkdir -p build
	$(CXX) -std=c++20 -O2 -Wall -Wextra -Werror -I$(CUDA_ROOT)/include -c transfer_contracts/transfer_backend_unavailable.cpp -o build/transfer-backend-unavailable.o

NIXL_ROOT ?=
# CPU contract tests can use synchronous POSIX; GPU images select NIXL at build
# time. There is no runtime fallback or per-transfer backend negotiation.
ifneq ($(NIXL_ROOT),)
TRANSFER_FLAGS := -DPAGEBROKER_NIXL -I$(NIXL_ROOT)/include -L$(NIXL_ROOT)/lib -lnixl -lnixl_build
TRANSFER_SOURCES := nixl_transfer.cpp
endif

.PHONY: test-cuda-transfer
test-cuda-transfer:
	mkdir -p build
	$(CXX) -O2 -std=c++20 -Wall -Wextra -Werror -I. -I$(CUDA_ROOT)/include \
		cuda_posix_transfer_test.cpp cuda_posix_transfer.cpp file_descriptor.cpp $(TRANSFER_SOURCES) \
		$(GTEST_FLAGS) -pthread $(TRANSFER_FLAGS) -o build/cuda-transfer-test
	./build/cuda-transfer-test

CUDA_LIB_DIR ?= $(CUDA_ROOT)/lib64/stubs
.PHONY: gpu-engine
gpu-engine: generate
	mkdir -p build
	$(CXX) -O2 -std=c++20 -Wall -Wextra -Werror -I. -I$(CUDA_ROOT)/include \
		gpu_engine_worker.cpp fd_transport.cpp v1/pagebroker.pb.cc gpu_engine.pb.cc \
		file_descriptor.cpp cuda_posix_transfer.cpp $(TRANSFER_SOURCES) storage_manifest.cpp \
		../cmd/cuda-checkpoint-helper/custom_storage.cpp \
		-L$(CUDA_LIB_DIR) -lcuda -lprotobuf -pthread $(TRANSFER_FLAGS) -o build/pagebroker-gpu-engine

.PHONY: test-gpu-engine
test-gpu-engine: generate
	mkdir -p build
	$(CXX) -std=c++20 -Wall -Wextra -Werror -I. fake_gpu_engine.cpp fd_transport.cpp \
		file_descriptor.cpp gpu_engine.pb.cc v1/pagebroker.pb.cc -lprotobuf -pthread -o build/fake-gpu-engine
	$(CXX) -std=c++20 -Wall -Wextra -Werror -I. gpu_engine_test.cpp gpu_engine.cpp fd_transport.cpp \
		file_descriptor.cpp gpu_engine.pb.cc v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -pthread -o build/gpu-engine-test
	./build/gpu-engine-test

.PHONY: test-cuda-helper
test-cuda-helper:
	mkdir -p build
	$(CXX) -O2 -std=c++20 -Wall -Wextra -Werror -I$(CUDA_ROOT)/include \
		../cmd/cuda-checkpoint-helper/custom_storage.cpp ../cmd/cuda-checkpoint-helper/custom_storage_test.cpp \
		$(GTEST_FLAGS) -pthread -o build/cuda-helper-test
	./build/cuda-helper-test
