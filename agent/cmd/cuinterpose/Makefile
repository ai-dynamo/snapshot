# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

BUILD_DIR ?= build
INTERPOSER := $(BUILD_DIR)/libcuinterposer.so
COORDINATOR := $(BUILD_DIR)/cuinterposer-coordinator
COMMON_FLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror
INTERPOSER_SOURCES := interpose.c multicast.c posix.c symbols.c util.c
INTERPOSER_HEADERS := multicast.h posix.h protocol.h symbols.h util.h
COORDINATOR_SOURCES := coordinator.c util.c
COORDINATOR_HEADERS := protocol.h util.h

.PHONY: all clean

all: $(INTERPOSER) $(COORDINATOR)

$(BUILD_DIR):
	mkdir -p $@

$(INTERPOSER): $(INTERPOSER_SOURCES) $(INTERPOSER_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) -fPIC -shared -o $@.tmp $(INTERPOSER_SOURCES)
	readelf -d $@.tmp >/dev/null
	mv $@.tmp $@

$(COORDINATOR): $(COORDINATOR_SOURCES) $(COORDINATOR_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) -static -o $@.tmp $(COORDINATOR_SOURCES)
	! readelf -d $@.tmp 2>/dev/null | grep -q NEEDED
	mv $@.tmp $@

clean:
	rm -rf $(BUILD_DIR)
