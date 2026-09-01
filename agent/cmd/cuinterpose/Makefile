# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

BUILD_DIR ?= build
CUDA_HOME ?= /usr/local/cuda
INTERPOSER := $(BUILD_DIR)/libcuinterposer.so
COORDINATOR := $(BUILD_DIR)/cuinterposer-coordinator
MULTICAST_FORWARD_TEST := $(BUILD_DIR)/multicast-forward-test
MIN_GLIBC := GLIBC_2.34
INTERPOSER_CPPFLAGS := -DCUINTERPOSER_DLSYM_VERSION=\"$(MIN_GLIBC)\"
COMMON_FLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror
INTERPOSER_SOURCES := interpose.c multicast.c posix.c symbols.c util.c
INTERPOSER_HEADERS := multicast.h posix.h protocol.h symbols.h util.h
COORDINATOR_SOURCES := coordinator.c util.c
COORDINATOR_HEADERS := protocol.h util.h

.PHONY: all clean test

all: $(INTERPOSER) $(COORDINATOR)

$(BUILD_DIR):
	mkdir -p $@

$(INTERPOSER): $(INTERPOSER_SOURCES) $(INTERPOSER_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) $(INTERPOSER_CPPFLAGS) \
		-fPIC -shared -Wno-deprecated-declarations -I$(CUDA_HOME)/include \
		-o $@.tmp $(INTERPOSER_SOURCES) -Wl,-Bsymbolic-functions -ldl -pthread
	readelf -d $@.tmp >/dev/null
	! readelf -d $@.tmp | grep -Eq 'NEEDED.*(libcuda|libcudart)'
	max=$$(readelf -V $@.tmp | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1); \
		[ "$$(printf '%s\n%s\n' "$$max" "$(MIN_GLIBC)" | sort -uV | tail -1)" = "$(MIN_GLIBC)" ]
	readelf --dyn-syms -W $@.tmp | grep -q 'dlvsym@$(MIN_GLIBC)'
	mv $@.tmp $@

$(COORDINATOR): $(COORDINATOR_SOURCES) $(COORDINATOR_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) -static -o $@.tmp $(COORDINATOR_SOURCES)
	! readelf -d $@.tmp 2>/dev/null | grep -q NEEDED
	mv $@.tmp $@

test: $(INTERPOSER)
	$(CC) $(COMMON_FLAGS) -fPIC -shared -Wno-deprecated-declarations \
		-I$(CUDA_HOME)/include -Wl,-soname,libcuda.so.1 \
		-o $(BUILD_DIR)/libcuda.so.1 tests/fake_cuda.c -ldl
	ln -sf libcuda.so.1 $(BUILD_DIR)/libcuda.so
	$(CC) $(COMMON_FLAGS) -fPIC -shared -Wno-deprecated-declarations \
		-I$(CUDA_HOME)/include -Wl,-soname,libcudart.so.13 \
		-o $(BUILD_DIR)/libcudart.so.13 tests/fake_cudart.c
	ln -sf libcudart.so.13 $(BUILD_DIR)/libcudart.so
	$(CC) $(COMMON_FLAGS) -Wno-deprecated-declarations -I$(CUDA_HOME)/include \
		-L$(BUILD_DIR) -Wl,-rpath,'$$ORIGIN' -o $(MULTICAST_FORWARD_TEST) \
		tests/multicast_forward_test.c -lcuda -lcudart -ldl
	mkdir -p $(BUILD_DIR)/control
	SNAPSHOT_CONTROL_DIR=$(abspath $(BUILD_DIR)/control) \
		LD_PRELOAD=$(abspath $(INTERPOSER)) $(MULTICAST_FORWARD_TEST)

clean:
	rm -rf $(BUILD_DIR)
