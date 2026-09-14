# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build from the agent directory so PageBroker and the shared CUDA core are
# both in context:
#   docker build -f pagebroker/Dockerfile .
ARG AGENT_BASE_IMAGE=nvcr.io/nvidia/cuda-dl-base:25.11-cuda13.0-devel-ubuntu24.04@sha256:8315e2455736c4f9b597f15c5fb4d31f834e798e0c6b66bbdbdbac491ce26bd1
ARG CUDA_HELPER_HEADERS_IMAGE=nvcr.io/nvidia/cuda-dl-base:26.08-cuda13.4-devel-ubuntu24.04@sha256:edd686dcb3e87806dfc869623ccb0a7e62c0d09bd720a82014c23efcecc8d9a2

FROM ${CUDA_HELPER_HEADERS_IMAGE} AS cuda-helper-headers

FROM ${AGENT_BASE_IMAGE} AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libgtest-dev \
    libprotobuf-dev \
    libssl-dev \
    pkg-config \
    protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src/pagebroker
COPY --from=cuda-helper-headers /usr/local/cuda-13.4/targets/x86_64-linux/include/cuda.h /opt/cuda-13.4/include/cuda.h
COPY pagebroker .
COPY cmd/cuda-checkpoint-helper ../cmd/cuda-checkpoint-helper
COPY cmd/cuinterpose ../cmd/cuinterpose
RUN ln -sf /usr/local/cuda/lib64/stubs/libcuda.so \
      /usr/local/cuda/lib64/stubs/libcuda.so.1 \
    && make -C ../cmd/cuinterpose BUILD_DIR=/cuinterpose-build \
      /cuinterpose-build/cuinterpose-coordinator \
    && make daemon \
    && make test

FROM ${AGENT_BASE_IMAGE}

RUN apt-get update && apt-get install -y --no-install-recommends libprotobuf32t64 libssl3t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/pagebroker/pagebroker /usr/local/bin/pagebroker
COPY --from=build /src/pagebroker/pagebroker-cuda-worker /usr/local/bin/pagebroker-cuda-worker
COPY --from=build /cuinterpose-build/cuinterpose-coordinator /usr/local/bin/cuinterpose-coordinator
ENTRYPOINT ["/usr/local/bin/pagebroker"]
