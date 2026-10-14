# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

CUDA_ROOT ?= /usr/local/cuda
CUDA_LIB_DIR ?= $(CUDA_ROOT)/lib64/stubs
CFLAGS := -O2 -Wall -Wextra -Werror

.DEFAULT_GOAL := helper
.PHONY: helper

helper: bin/cuda-checkpoint-helper

bin/cuda-checkpoint-helper: main.c
	mkdir -p bin
	$(CC) $(CFLAGS) -I$(CUDA_ROOT)/include $< -L$(CUDA_LIB_DIR) -lcuda -o $@
