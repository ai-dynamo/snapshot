# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROTO := v1/pagebroker.proto
GTEST_FLAGS := $(shell pkg-config --cflags --libs gtest_main)
BROKER_SOURCES := broker.cpp checkpoint_archive.cpp checkpoint_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp file_descriptor.cpp

.PHONY: daemon generate test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

# zlib links dynamically, against the runtime base image's own libz.so.1
# (already present -- see checkpoint_archive.cpp), not statically like
# protobuf: it's part of the base image's existing footprint, so no new
# third-party source/attribution to carry here.
daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc -Wl,-Bstatic -lprotobuf -Wl,-Bdynamic -lz -o pagebroker

test: generate $(BROKER_SOURCES) daemon_test.cpp
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) daemon_test.cpp v1/pagebroker.pb.cc -lprotobuf -lz $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test
