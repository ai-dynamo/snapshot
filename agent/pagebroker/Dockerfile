# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG DOCKER_PROXY
ARG PAGEBROKER_BUILD_IMAGE=ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254
# When bumping the base, update the source URL in compliance/README.source.txt
# to the matching version.
ARG PAGEBROKER_BASE_IMAGE=nvcr.io/nvidia/distroless/cc:v4.0.8@sha256:8003f23a7122e630e3a78875dda6e68403c7ccc0adb393470a8c7739fafa4a02

ARG CUDA_HEADERS_IMAGE=nvcr.io/nvidia/cuda-dl-base:26.08-cuda13.4-devel-ubuntu24.04@sha256:edd686dcb3e87806dfc869623ccb0a7e62c0d09bd720a82014c23efcecc8d9a2

FROM ${CUDA_HEADERS_IMAGE} AS cuda-headers

FROM ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254 AS nixl
# POSIX-only NIXL at the qualified transfer implementation revision.
ARG NIXL_REF=71ca3cb249a481ae892fcc042a456e1a9b87463d
ARG BUILD_JOBS=4
ARG NVTX_REF=3ebbc93ded7285963bff932c678fa367eb393ba6
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates git libaio-dev meson ninja-build pkg-config pybind11-dev python3 \
    && rm -rf /var/lib/apt/lists/*
RUN git init /tmp/nvtx && cd /tmp/nvtx \
    && git remote add origin https://github.com/NVIDIA/NVTX.git \
    && git fetch --depth 1 origin ${NVTX_REF} \
    && test "$(git rev-parse FETCH_HEAD)" = "${NVTX_REF}" \
    && git checkout --detach ${NVTX_REF} \
    && mkdir -p /nixl-install/usr/local/include \
    && cp -a c/include/nvtx3 /nixl-install/usr/local/include/
RUN git init /tmp/nixl && cd /tmp/nixl \
    && git remote add origin https://github.com/ai-dynamo/nixl.git \
    && git fetch --depth 1 origin ${NIXL_REF} \
    && test "$(git rev-parse FETCH_HEAD)" = "${NIXL_REF}" \
    && git checkout --detach ${NIXL_REF} \
    && meson setup build --prefix=/usr/local --libdir=lib --buildtype=release \
      -Dbuild_tests=false -Dbuild_examples=false -Denable_plugins=POSIX \
    && meson compile -C build -j ${BUILD_JOBS} \
    && DESTDIR=/nixl-install meson install -C build --strip \
    && mkdir -p /nixl-runtime/lib/plugins \
    && cp /nixl-install/usr/local/lib/libnixl*.so /nixl-runtime/lib/ \
    && cp /nixl-install/usr/local/lib/libserdes.so /nixl-install/usr/local/lib/libstream.so \
      /nixl-install/usr/local/lib/libfile_utils.so /nixl-runtime/lib/ \
    && cp -a /nixl-install/usr/local/lib/liburing.so* /nixl-runtime/lib/ \
    && cp /nixl-install/usr/local/lib/plugins/libplugin_POSIX.so /nixl-runtime/lib/plugins/

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libaio-dev \
    libgtest-dev \
    libprotobuf-dev \
    pkg-config \
    protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

COPY --from=cuda-headers /usr/local/cuda/include/ /opt/cuda/include/
COPY --from=cuda-headers /usr/local/cuda/lib64/stubs/ /opt/cuda/lib64/stubs/
COPY --from=nixl /nixl-install/usr/local/ /usr/local/
COPY --from=nixl /nixl-runtime/lib/ /nixl-runtime-check/
ENV LD_LIBRARY_PATH=/usr/local/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
ENV NIXL_PLUGIN_DIR=/usr/local/lib/plugins

WORKDIR /src
COPY . .
# Run CPU tests and compile GPU tests for GPU CI.
RUN make test CUDA_ROOT=/opt/cuda NIXL_ROOT=/usr/local \
    && make build/gpu-checkpoint-test build/gpu-transfer-test CUDA_ROOT=/opt/cuda CUDA_LIBRARY_DIR=/opt/cuda/lib64/stubs \
    && make daemon CUDA_ROOT=/opt/cuda NIXL_ROOT=/usr/local

# Smoke-test must run here: the runtime stage is distroless (no shell).
RUN output=$(/src/pagebroker 2>&1); status=$?; \
    [ "$status" -eq 2 ]; \
    [ "$output" = "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests" ]

RUN mv /usr/local/lib /usr/local/lib-build \
    && mkdir -p /runtime-lib \
    && cp -a /usr/lib/x86_64-linux-gnu/libaio.so.* /runtime-lib/ \
    && for library in /nixl-runtime-check/libnixl*.so /nixl-runtime-check/plugins/libplugin_POSIX.so; do \
         LD_LIBRARY_PATH=/nixl-runtime-check:/runtime-lib ldd "$library" > /tmp/dependencies \
         && ! grep -q 'not found' /tmp/dependencies || { cat /tmp/dependencies; exit 1; }; \
       done \
    && mv /usr/local/lib-build /usr/local/lib

# Corresponding source for the statically linked protobuf. Fetched in this
# stage, from the same libprotobuf-dev install that provided the linked
# libprotobuf.a, and pinned to its source version (see compliance/).
RUN sh /src/compliance/collect-sources.sh /legal/source/protobuf \
    && rm -rf /var/lib/apt/lists/*

# Assemble the legal materials so the runtime stage (distroless, no shell) can
# receive them in a single COPY.
FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS sources

COPY --from=build /legal/source/protobuf /legal/source/protobuf
COPY --from=build /src /legal/source/pagebroker
COPY compliance/README.source.txt /legal/source/README.txt

RUN rm -rf /legal/source/pagebroker/build \
           /legal/source/pagebroker/pagebroker \
           /legal/source/pagebroker/v1/pagebroker.pb.*

# Attribution generated from the collected source, so it cannot drift from the
# version actually shipped.
COPY compliance/gen-attribution.sh /tmp/gen-attribution.sh
RUN sh /tmp/gen-attribution.sh /legal/source/protobuf /legal/THIRD-PARTY.txt

FROM ${PAGEBROKER_BASE_IMAGE}

COPY --from=build /src/pagebroker /usr/local/bin/pagebroker
COPY --from=build /runtime-lib/ /usr/local/lib/
COPY --from=nixl /nixl-runtime/lib/ /usr/local/lib/
COPY --from=sources /legal /legal
ENV LD_LIBRARY_PATH=/usr/local/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
ENV NIXL_PLUGIN_DIR=/usr/local/lib/plugins

ENTRYPOINT ["/usr/local/bin/pagebroker"]
