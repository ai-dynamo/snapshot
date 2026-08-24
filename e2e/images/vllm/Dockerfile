# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG BASE_IMAGE=vllm/vllm-openai:v0.27.1
FROM ${BASE_IMAGE}

USER root

RUN set -eux; \
    printf 'deb http://archive.ubuntu.com/ubuntu noble main universe\n' \
      >/etc/apt/sources.list.d/snapshot-noble.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends -t noble libc6 libc-bin; \
    rm -f /etc/apt/sources.list.d/snapshot-noble.list; \
    rm -rf /var/lib/apt/lists/*
