# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

BUILD_DIR ?= build
INTERPOSER := $(BUILD_DIR)/libcuinterposer.so

.PHONY: all clean

all: $(INTERPOSER)

$(BUILD_DIR):
	mkdir -p $@

$(INTERPOSER): empty.c | $(BUILD_DIR)
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -fPIC -shared -o $@.tmp $<
	readelf -d $@.tmp >/dev/null
	mv $@.tmp $@

clean:
	rm -rf $(BUILD_DIR)
