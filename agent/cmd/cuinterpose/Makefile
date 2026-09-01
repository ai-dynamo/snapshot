# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

BUILD_DIR ?= build
CUDA_HOME ?= /usr/local/cuda
INTERPOSER := $(BUILD_DIR)/libcuinterposer.so
FORWARD_TEST := $(BUILD_DIR)/forward-test
MIN_GLIBC := GLIBC_2.34
INTERPOSER_CPPFLAGS := -DCUINTERPOSER_DLSYM_VERSION=\"$(MIN_GLIBC)\"
SOURCES := interpose.c symbols.c
HEADERS := symbols.h

.PHONY: all clean test

all: $(INTERPOSER)

$(BUILD_DIR):
	mkdir -p $@

$(INTERPOSER): $(SOURCES) $(HEADERS) | $(BUILD_DIR)
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror $(INTERPOSER_CPPFLAGS) \
		-fPIC -shared -Wno-deprecated-declarations -I$(CUDA_HOME)/include \
		-o $@.tmp $(SOURCES) -Wl,-Bsymbolic-functions -ldl -pthread
	readelf -d $@.tmp >/dev/null
	! readelf -d $@.tmp | grep -Eq 'NEEDED.*(libcuda|libcudart)'
	max=$$(readelf -V $@.tmp | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1); \
		[ "$$(printf '%s\n%s\n' "$$max" "$(MIN_GLIBC)" | sort -uV | tail -1)" = "$(MIN_GLIBC)" ]
	readelf --dyn-syms -W $@.tmp | grep -q 'dlvsym@$(MIN_GLIBC)'
	mv $@.tmp $@

test: $(INTERPOSER)
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -fPIC -shared \
		-Wno-deprecated-declarations -I$(CUDA_HOME)/include \
		-Wl,-soname,libcuda.so.1 -o $(BUILD_DIR)/libcuda.so.1 tests/fake_cuda.c -ldl
	ln -sf libcuda.so.1 $(BUILD_DIR)/libcuda.so
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -fPIC -shared \
		-Wno-deprecated-declarations -I$(CUDA_HOME)/include \
		-Wl,-soname,libcudart.so.13 -o $(BUILD_DIR)/libcudart.so.13 tests/fake_cudart.c
	ln -sf libcudart.so.13 $(BUILD_DIR)/libcudart.so
	$(CC) -std=gnu11 -O2 -Wall -Wextra -Werror -Wno-deprecated-declarations \
		-I$(CUDA_HOME)/include -L$(BUILD_DIR) -Wl,-rpath,'$$ORIGIN' \
		-o $(FORWARD_TEST) tests/forward_test.c -lcuda -lcudart -ldl
	LD_PRELOAD=$(abspath $(INTERPOSER)) $(FORWARD_TEST)

clean:
	rm -rf $(BUILD_DIR)
