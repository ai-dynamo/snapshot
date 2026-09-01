# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

BUILD_DIR ?= build
CUDA_HOME ?= /usr/local/cuda
INTERPOSER := $(BUILD_DIR)/libcuinterposer.so
COORDINATOR := $(BUILD_DIR)/cuinterposer-coordinator
MIN_GLIBC := GLIBC_2.34
INTERPOSER_CPPFLAGS := -DCUINTERPOSER_DLSYM_VERSION=\"$(MIN_GLIBC)\"
COMMON_FLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror
INTERPOSER_SOURCES := interpose.c posix.c symbols.c util.c
INTERPOSER_HEADERS := posix.h protocol.h symbols.h util.h
COORDINATOR_SOURCES := coordinator.c util.c
COORDINATOR_HEADERS := protocol.h util.h

.PHONY: all clean

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

clean:
	rm -rf $(BUILD_DIR)
