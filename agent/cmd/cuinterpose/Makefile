# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := frontend
.PHONY: frontend

frontend:
	mkdir -p build
	/usr/bin/gcc -std=gnu11 -O2 -Wall -Wextra -Werror -shared -fPIC -fvisibility=hidden \
		-Wl,-Bsymbolic-functions -I. frontend/frontend.c -o build/libcuinterpose.so -ldl -pthread
	chmod 0644 build/libcuinterpose.so
