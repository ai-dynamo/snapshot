# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0
#
# Builds libcuinterpose.so (the LD_PRELOAD shim) and cuinterpose-coordinator
# (a static helper the snapshot agent runs), and runs the unit tests against a
# fake libcuda/libcudart so no GPU is needed.
#
#   make                 build both artifacts into $(BUILD_DIR)
#   make test            build the fakes and GoogleTest binaries, run them
#   make CUDA_HOME=...   point at a CUDA toolkit (headers only are used)
#
# The shim is delivered in layers. Every C file in this directory is part of
# the shim except coordinator.c; tests are found by name (see "Tests" below).
# Adding a layer means adding files, not editing this Makefile.

BUILD_DIR ?= build
CUDA_HOME ?= /usr/local/cuda
CC ?= cc
CXX ?= c++

INTERPOSER := $(BUILD_DIR)/libcuinterpose.so
COORDINATOR := $(BUILD_DIR)/cuinterpose-coordinator

# The shim must load into any workload image from Ubuntu 22.04 (glibc 2.35)
# onward. It resolves glibc's dlsym through dlvsym(..., "GLIBC_2.34"), and the
# build rejects any symbol version newer than this.
MIN_GLIBC := GLIBC_2.34
# CUDA 13.1 added the device-explicit multicast bind entry points. The shipped
# shim is built against 13.1 or newer headers so those wrappers exist; the
# shim has no runtime dependency on the header version because every driver
# symbol is looked up at run time. Set ALLOW_OLD_CUDA_HEADERS=1 for local
# builds against older headers.
MIN_CUDA_VERSION := 13010
CUDA_VERSION := $(shell sed -n 's/^\#define CUDA_VERSION \([0-9]*\).*/\1/p' $(CUDA_HOME)/include/cuda.h 2>/dev/null)

COMMON_FLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror
INTERPOSER_CPPFLAGS := -DCUINTERPOSE_DLSYM_VERSION=\"$(MIN_GLIBC)\" -I$(CUDA_HOME)/include
INTERPOSER_CFLAGS := $(COMMON_FLAGS) -fPIC -fvisibility=hidden -Wno-deprecated-declarations
# -z defs: every reference must resolve at link time, so an accidental direct
# call into libcuda (which the shim must never link) fails the build instead of
# failing inside a workload. -Bsymbolic-functions: the shim's own internal calls
# bind locally and cannot be interposed by the application.
INTERPOSER_LDFLAGS := -shared -Wl,-Bsymbolic-functions -Wl,-z,defs -ldl -pthread
INTERPOSER_SOURCES := $(filter-out coordinator.c,$(wildcard *.c))
INTERPOSER_HEADERS := $(wildcard *.h)
COORDINATOR_SOURCES := coordinator.c util.c
COORDINATOR_HEADERS := protocol.h util.h
# Everything the shim is allowed to export. Anything else is a bug: an
# LD_PRELOAD library sits first in the process's symbol search order.
EXPORT_ALLOWLIST := '^(cu[A-Z][A-Za-z0-9_]*|cuda[A-Z][A-Za-z0-9_]*|dlsym|cuinterpose_[a-z_]+)$$'

.PHONY: all clean test check-cuda-headers

all: $(INTERPOSER) $(COORDINATOR)

$(BUILD_DIR):
	mkdir -p $@

check-cuda-headers:
	@test -n "$(CUDA_VERSION)" || { echo "cuda.h not found under $(CUDA_HOME)/include" >&2; exit 1; }
	@if [ "$(CUDA_VERSION)" -lt "$(MIN_CUDA_VERSION)" ] && [ -z "$(ALLOW_OLD_CUDA_HEADERS)" ]; then \
	  echo "CUDA headers are $(CUDA_VERSION); the shim is built against >= $(MIN_CUDA_VERSION) (set ALLOW_OLD_CUDA_HEADERS=1 to override)" >&2; exit 1; fi

$(INTERPOSER): $(INTERPOSER_SOURCES) $(INTERPOSER_HEADERS) | $(BUILD_DIR) check-cuda-headers
	$(CC) $(INTERPOSER_CFLAGS) $(INTERPOSER_CPPFLAGS) -o $@.tmp $(INTERPOSER_SOURCES) $(INTERPOSER_LDFLAGS)
	@# No link-time dependency on the CUDA libraries.
	! readelf -d $@.tmp | grep -Eq 'NEEDED.*(libcuda|libcudart)'
	@# No glibc symbol newer than MIN_GLIBC.
	max=$$(readelf -V $@.tmp | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1); \
	  [ "$$(printf '%s\n%s\n' "$$max" "$(MIN_GLIBC)" | sort -uV | tail -1)" = "$(MIN_GLIBC)" ]
	@# The real dlsym is fetched through the pinned dlvsym (once the shim resolves symbols at all).
	@if [ -f symbols.c ]; then readelf --dyn-syms -W $@.tmp | grep -q 'dlvsym@$(MIN_GLIBC)'; fi
	@# Only the intended symbols are exported.
	@bad=$$(nm -D --defined-only $@.tmp | awk '$$2 ~ /^[TWDRBV]$$/ {print $$3}' | grep -Ev $(EXPORT_ALLOWLIST) || true); \
	  if [ -n "$$bad" ]; then echo "unexpected exported symbols:" $$bad >&2; exit 1; fi
	mv $@.tmp $@

$(COORDINATOR): $(COORDINATOR_SOURCES) $(COORDINATOR_HEADERS) | $(BUILD_DIR)
	$(CC) $(COMMON_FLAGS) -static -pthread -o $@.tmp $(COORDINATOR_SOURCES)
	! readelf -d $@.tmp 2>/dev/null | grep -q NEEDED
	mv $@.tmp $@

# ---------------------------------------------------------------------------
# Tests. The shim under test is a separate build with AddressSanitizer and
# UndefinedBehaviorSanitizer; the production .so above is never instrumented.
# Fake libcuda.so.1 / libcudart.so.13 record what they were called with.
#
# Three kinds of test, told apart by file name:
#   tests/<name>_unit_test.cc     links the shim's CUDA-free objects (util,
#                                 posix, tables, export cache) and runs alone
#   tests/<name>_preload_test.cc  runs with the sanitized shim LD_PRELOADed
#                                 over the fake libcuda/libcudart
#   tests/coordinator_test.cc     runs the sanitized coordinator binary
#                                 against fake participants
# ---------------------------------------------------------------------------
TEST_DIR := $(BUILD_DIR)/test
SANITIZE ?= address,undefined
SANITIZE_FLAGS := $(if $(SANITIZE),-fsanitize=$(SANITIZE) -fno-omit-frame-pointer,)
GTEST_CFLAGS ?= $(shell pkg-config --cflags gtest 2>/dev/null)
GTEST_LIBS ?= $(shell pkg-config --libs gtest_main gtest 2>/dev/null || echo -lgtest_main -lgtest)
TEST_CXXFLAGS := -std=c++17 -O1 -g -Wall -Wextra -Werror -Wno-deprecated-declarations -I. -I$(CUDA_HOME)/include $(GTEST_CFLAGS)
TEST_INTERPOSER := $(TEST_DIR)/libcuinterpose.so
FAKE_CUDA := $(TEST_DIR)/libcuda.so.1
FAKE_CUDART := $(TEST_DIR)/libcudart.so.13
TEST_COORDINATOR := $(TEST_DIR)/cuinterpose-coordinator
TEST_HEADERS := $(wildcard tests/*.h)
UNIT_TESTS := $(patsubst tests/%.cc,$(TEST_DIR)/%,$(wildcard tests/*_unit_test.cc))
PRELOAD_TESTS := $(patsubst tests/%.cc,$(TEST_DIR)/%,$(wildcard tests/*_preload_test.cc))
COORDINATOR_TEST := $(patsubst tests/%.cc,$(TEST_DIR)/%,$(wildcard tests/coordinator_test.cc))
# Shim sources that need no CUDA driver at run time. symbols.c and context.c
# resolve driver symbols and are exercised through the preload tests.
UNIT_SOURCES := $(filter-out coordinator.c interpose.c multicast.c symbols.c context.c,$(wildcard *.c))
UNIT_OBJECTS := $(patsubst %.c,$(TEST_DIR)/%.o,$(UNIT_SOURCES))
# When the shim under test is ASan-instrumented, the ASan runtime must be the
# first library in the process, ahead of the shim itself.
ASAN_PRELOAD := $(if $(findstring address,$(SANITIZE)),$(shell $(CC) -print-file-name=libasan.so):,)
TEST_ENV := SNAPSHOT_CONTROL_DIR=$(abspath $(TEST_DIR)/control) CUINTERPOSE_COORDINATOR=$(abspath $(TEST_COORDINATOR))

$(TEST_DIR):
	mkdir -p $@

$(TEST_DIR)/%.o: %.c $(INTERPOSER_HEADERS) | $(TEST_DIR)
	$(CC) $(COMMON_FLAGS) $(SANITIZE_FLAGS) -g -c -o $@ $<

# symbols.c is compiled without instrumentation: it defines dlsym, which the
# sanitizer runtime itself calls while it is still initializing, before
# instrumented code may run.
SYMBOLS_OBJECT := $(if $(wildcard symbols.c),$(TEST_DIR)/symbols.plain.o,)
SANITIZED_SOURCES := $(filter-out symbols.c,$(INTERPOSER_SOURCES))
$(TEST_DIR)/symbols.plain.o: symbols.c $(INTERPOSER_HEADERS) | $(TEST_DIR) check-cuda-headers
	$(CC) $(INTERPOSER_CFLAGS) $(INTERPOSER_CPPFLAGS) -g -c -o $@ symbols.c

$(TEST_INTERPOSER): $(INTERPOSER_SOURCES) $(INTERPOSER_HEADERS) $(SYMBOLS_OBJECT) | $(TEST_DIR) check-cuda-headers
	$(CC) $(INTERPOSER_CFLAGS) $(INTERPOSER_CPPFLAGS) $(SANITIZE_FLAGS) -g -o $@ $(SANITIZED_SOURCES) $(SYMBOLS_OBJECT) $(INTERPOSER_LDFLAGS)

$(FAKE_CUDA): tests/fake_cuda.c tests/fake_cuda.h | $(TEST_DIR)
	$(CC) $(COMMON_FLAGS) -fPIC -shared -Wno-deprecated-declarations -I$(CUDA_HOME)/include \
	  -Wl,-soname,libcuda.so.1 -o $@ tests/fake_cuda.c -pthread
	ln -sf libcuda.so.1 $(TEST_DIR)/libcuda.so

$(FAKE_CUDART): tests/fake_cudart.c tests/fake_cuda.h | $(TEST_DIR) $(FAKE_CUDA)
	$(CC) $(COMMON_FLAGS) -fPIC -shared -Wno-deprecated-declarations -I$(CUDA_HOME)/include \
	  -Wl,-soname,libcudart.so.13 -L$(TEST_DIR) -o $@ tests/fake_cudart.c -lcuda
	ln -sf libcudart.so.13 $(TEST_DIR)/libcudart.so

$(TEST_DIR)/%_unit_test: tests/%_unit_test.cc $(UNIT_OBJECTS) $(INTERPOSER_HEADERS) $(TEST_HEADERS) | $(TEST_DIR)
	$(CXX) $(TEST_CXXFLAGS) $(SANITIZE_FLAGS) -o $@ $< $(UNIT_OBJECTS) $(GTEST_LIBS) -pthread

$(TEST_DIR)/%_preload_test: tests/%_preload_test.cc $(INTERPOSER_HEADERS) $(TEST_HEADERS) $(FAKE_CUDA) $(FAKE_CUDART) | $(TEST_DIR)
	$(CXX) $(TEST_CXXFLAGS) $(SANITIZE_FLAGS) -L$(TEST_DIR) -Wl,-rpath,'$$ORIGIN' -o $@ $< -lcuda -lcudart -ldl $(GTEST_LIBS) -pthread

# A dynamically linked, sanitizer-instrumented coordinator for the tests; the
# production one above is static and uninstrumented.
$(TEST_COORDINATOR): $(COORDINATOR_SOURCES) $(COORDINATOR_HEADERS) | $(TEST_DIR)
	$(CC) $(COMMON_FLAGS) $(SANITIZE_FLAGS) -g -pthread -o $@ $(COORDINATOR_SOURCES)

$(TEST_DIR)/coordinator_test: tests/coordinator_test.cc $(TEST_DIR)/util.o protocol.h util.h $(TEST_HEADERS) $(TEST_COORDINATOR) | $(TEST_DIR)
	$(CXX) $(TEST_CXXFLAGS) $(SANITIZE_FLAGS) -o $@ $< $(TEST_DIR)/util.o $(GTEST_LIBS) -pthread

test: $(INTERPOSER) $(COORDINATOR) $(UNIT_TESTS) $(COORDINATOR_TEST) $(if $(PRELOAD_TESTS),$(TEST_INTERPOSER) $(TEST_COORDINATOR)) $(PRELOAD_TESTS)
	@mkdir -p $(TEST_DIR)/control
	$(foreach t,$(UNIT_TESTS),$(t) &&) true
	$(foreach t,$(COORDINATOR_TEST),CUINTERPOSE_COORDINATOR=$(abspath $(TEST_COORDINATOR)) $(t) &&) true
	$(foreach t,$(PRELOAD_TESTS),$(TEST_ENV) LD_PRELOAD=$(ASAN_PRELOAD)$(abspath $(TEST_INTERPOSER)) $(t) &&) true

clean:
	rm -rf $(BUILD_DIR)
