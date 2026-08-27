# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

CC ?= cc
CUDA_HOME ?= /usr/local/cuda
BUILD_DIR ?= build

INTERPOSER := $(BUILD_DIR)/libcuinterposer.so
COORDINATOR := $(BUILD_DIR)/cuinterposer-coordinator
INTERPOSER_SOURCES := interpose.c multicast.c posix.c symbols.c util.c
INTERPOSER_HEADERS := multicast.h posix.h protocol.h symbols.h util.h
COORDINATOR_SOURCES := coordinator.c util.c
COORDINATOR_HEADERS := protocol.h util.h
# -std=gnu11 keeps strtol off the C23 __isoc23_strtol@GLIBC_2.38 redirect.
COMMON_FLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror

# The shim is preloaded into workload images, so its glibc requirement is a
# compatibility contract. 2.34 is the libdl-into-libc merge that versions dlsym
# and dlvsym; it covers Ubuntu 22.04 and later. A rise is invisible in source,
# so assert it. The coordinator is static and exempt.
#
# Building below 2.34 is also unsupported: dlvsym is then unversioned, and the
# GLIBC_2.34 lookup in symbols.c would return NULL at runtime, leaving the shim
# loaded but inert. That direction is asserted separately, since a lower floor
# passes the check below.
MIN_GLIBC := GLIBC_2.34
INTERPOSER_CPPFLAGS := -DCUINTERPOSER_DLSYM_VERSION=\"$(MIN_GLIBC)\"

define assert_glibc_floor
	max=$$(readelf -V $(1) | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1); \
	if [ -n "$$max" ] && [ "$$(printf '%s\n%s\n' "$$max" "$(MIN_GLIBC)" | sort -uV | tail -1)" != "$(MIN_GLIBC)" ]; then \
		echo "ERROR: $(1) requires $$max, above the $(MIN_GLIBC) floor" >&2; \
		exit 1; \
	fi
endef

.PHONY: all clean

all: $(INTERPOSER) $(COORDINATOR)

$(BUILD_DIR):
	mkdir -p $@

$(INTERPOSER): $(INTERPOSER_SOURCES) $(INTERPOSER_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) $(INTERPOSER_CPPFLAGS) -fPIC -shared -Wno-deprecated-declarations \
		-I$(CUDA_HOME)/include -o $@.tmp $(INTERPOSER_SOURCES) \
		-Wl,-Bsymbolic-functions -ldl -pthread
	readelf -d $@.tmp >/dev/null
	! readelf -d $@.tmp | grep -Eq 'NEEDED.*(libcuda|libcudart)'
	$(call assert_glibc_floor,$@.tmp)
	readelf --dyn-syms -W $@.tmp | grep -q 'dlvsym@$(MIN_GLIBC)'
	mv $@.tmp $@

$(COORDINATOR): $(COORDINATOR_SOURCES) $(COORDINATOR_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) -static -o $@.tmp $(COORDINATOR_SOURCES)
	! readelf -d $@.tmp 2>/dev/null | grep -q NEEDED
	mv $@.tmp $@

clean:
	rm -rf $(BUILD_DIR)
