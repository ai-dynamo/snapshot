# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROTO := v1/pagebroker.proto
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
MODEL_STREAMER_LIB_DIR ?= /usr/local/lib
MODEL_STREAMER_FLAGS := -L$(MODEL_STREAMER_LIB_DIR) -lstreamer -Wl,-rpath,$(MODEL_STREAMER_LIB_DIR)
CRYPTO_FLAGS = $(shell pkg-config --cflags --libs libcrypto)
RESTORE_SOURCES := file_descriptor.cpp transfer/engine/model_streamer/model_streamer_restore.cpp utils/event_loop.cpp utils/sha256.cpp
BROKER_SOURCES := $(RESTORE_SOURCES) broker.cpp checkpoint_transaction_descriptor.cpp restore_transaction_descriptor.cpp transaction.cpp transfer/filesystem/filesystem_storage.cpp transfer/engine/model_streamer/model_streamer_transfer_engine.cpp transfer/engine/posix/posix_copy_engine.cpp transfer/engine/transfer_engine.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp
TEST_SOURCES := tests/storage_contract_test.cpp tests/daemon_test.cpp utils/tests/event_loop_test.cpp utils/tests/sha256_test.cpp transfer/engine/model_streamer/tests/model_streamer_restore_test.cpp tests/temporary_directory_test.cpp
MODEL_STREAMER_RECOVERY_TEST_SOURCES := $(RESTORE_SOURCES) transfer/filesystem/filesystem_storage.cpp transfer/engine/model_streamer/model_streamer_transfer_engine.cpp transfer/engine/model_streamer/tests/model_streamer_transfer_engine_test.cpp transfer/engine/transfer_engine.cpp

.PHONY: daemon generate test s3-test-build s3-test s3-fixture-test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc -Wl,-Bstatic -lprotobuf -Wl,-Bdynamic $(MODEL_STREAMER_FLAGS) $(CRYPTO_FLAGS) -o pagebroker

test: generate $(BROKER_SOURCES) $(TEST_SOURCES) $(MODEL_STREAMER_RECOVERY_TEST_SOURCES)
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) $(TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(MODEL_STREAMER_FLAGS) $(CRYPTO_FLAGS) -o build/pagebroker-test
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" ./build/pagebroker-test
	c++ -I. -std=c++20 -Wall -Werror $(MODEL_STREAMER_RECOVERY_TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(CRYPTO_FLAGS) -o build/model-streamer-recovery-test
	./build/model-streamer-recovery-test

# Uses a disposable local S3 service; no cloud credentials are needed.
s3-test-build:
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(RESTORE_SOURCES) transfer/engine/model_streamer/tests/model_streamer_s3_test.cpp $(GTEST_FLAGS) $(MODEL_STREAMER_FLAGS) $(CRYPTO_FLAGS) -o build/model-streamer-s3-test

s3-test: s3-test-build
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" python3 tests/s3_integration.py --binary ./build/model-streamer-s3-test

s3-fixture-test:
	python3 -m unittest discover -s tests -p 'test_*.py' -v
