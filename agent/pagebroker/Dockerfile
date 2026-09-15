# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build from the agent directory so PageBroker and the shared CUDA core are
# both in context:
#   docker build -f pagebroker/Dockerfile .
ARG AGENT_BASE_IMAGE=nvcr.io/nvidia/cuda-dl-base:25.11-cuda13.0-devel-ubuntu24.04@sha256:8315e2455736c4f9b597f15c5fb4d31f834e798e0c6b66bbdbdbac491ce26bd1
ARG CUDA_HELPER_HEADERS_IMAGE=nvcr.io/nvidia/cuda-dl-base:26.08-cuda13.4-devel-ubuntu24.04@sha256:edd686dcb3e87806dfc869623ccb0a7e62c0d09bd720a82014c23efcecc8d9a2
ARG NIXL_REPO=https://github.com/ai-dynamo/nixl.git
# Immutable pin containing the POSIX short-write/error recovery used by the
# PageBroker storage adapter.
ARG NIXL_REF=71ca3cb249a481ae892fcc042a456e1a9b87463d

FROM ${CUDA_HELPER_HEADERS_IMAGE} AS cuda-helper-headers

FROM ${AGENT_BASE_IMAGE} AS nixl-builder

ARG NIXL_REPO
ARG NIXL_REF

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    git \
    libaio-dev \
    meson \
    ninja-build \
    pkg-config \
    pybind11-dev \
    python3 \
    && rm -rf /var/lib/apt/lists/*

RUN git init /tmp/nixl \
    && cd /tmp/nixl \
    && git remote add origin ${NIXL_REPO} \
    && git fetch --depth 1 origin ${NIXL_REF} \
    && test "$(git rev-parse FETCH_HEAD)" = "${NIXL_REF}" \
    && git checkout --detach ${NIXL_REF} \
    && CXXFLAGS="-Wno-error=maybe-uninitialized -Wno-maybe-uninitialized" meson setup build-test \
      --buildtype=debugoptimized \
      -Dbuild_tests=true \
      -Dbuild_examples=false \
      -Denable_plugins=POSIX \
    && meson compile -C build-test \
    && NIXL_PLUGIN_DIR=/tmp/nixl/build-test/src/plugins/posix \
      ./build-test/test/unit/plugins/posix/nixl_posix_test -n 32 \
    && meson setup build \
      --prefix=/usr/local \
      --libdir=lib \
      --buildtype=release \
      -Dbuild_tests=false \
      -Dbuild_examples=false \
      -Denable_plugins=POSIX \
    && meson compile -C build \
    && DESTDIR=/nixl-install meson install -C build --strip \
    && mkdir -p /nixl-runtime/lib/plugins \
    && printf '%s\n' "${NIXL_REF}" > /nixl-runtime/NIXL_COMMIT \
    && cp /tmp/nixl/subprojects/liburing-liburing-2.14/LICENSE /nixl-runtime/liburing-LICENSE \
    && cp \
      /nixl-install/usr/local/lib/libnixl.so \
      /nixl-install/usr/local/lib/libnixl_build.so \
      /nixl-install/usr/local/lib/libnixl_common.so \
      /nixl-install/usr/local/lib/libserdes.so \
      /nixl-install/usr/local/lib/libstream.so \
      /nixl-install/usr/local/lib/libfile_utils.so \
      /nixl-runtime/lib/ \
    && cp -a /nixl-install/usr/local/lib/liburing.so* /nixl-runtime/lib/ \
    && cp /nixl-install/usr/local/lib/plugins/libplugin_POSIX.so /nixl-runtime/lib/plugins/

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

FROM build AS build-nixl

RUN apt-get update && apt-get install -y --no-install-recommends libaio1t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=nixl-builder /nixl-install/usr/local /usr/local

RUN make daemon-nixl \
    && NIXL_PLUGIN_DIR=/usr/local/lib/plugins \
      LD_LIBRARY_PATH=/usr/local/lib:/usr/local/cuda/lib64/stubs \
      make test-nixl

# Explicit qualification target. The final stage below remains the default
# direct-POSIX PageBroker image until the NIXL adapter is qualified.
FROM ${AGENT_BASE_IMAGE} AS nixl

RUN apt-get update && apt-get install -y --no-install-recommends \
      libaio1t64 libprotobuf32t64 libssl3t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build-nixl /src/pagebroker/pagebroker /usr/local/bin/pagebroker
COPY --from=build-nixl /src/pagebroker/pagebroker-cuda-worker /usr/local/bin/pagebroker-cuda-worker
COPY --from=build-nixl /cuinterpose-build/cuinterpose-coordinator /usr/local/bin/cuinterpose-coordinator
COPY --from=build-nixl /src/pagebroker/build/cuda-nixl-transfer-test /tmp/cuda-nixl-transfer-test
COPY --from=nixl-builder /nixl-runtime/lib /usr/local/lib
COPY --from=nixl-builder /nixl-runtime/NIXL_COMMIT /usr/local/share/nixl/NIXL_COMMIT
COPY --from=nixl-builder /tmp/nixl/LICENSE /legal/NIXL/LICENSE
COPY --from=nixl-builder /tmp/nixl/ATTRIBUTIONS-CPP.md /legal/NIXL/ATTRIBUTIONS-CPP.md
COPY --from=nixl-builder /nixl-runtime/liburing-LICENSE /legal/liburing/LICENSE

ENV NIXL_PLUGIN_DIR=/usr/local/lib/plugins

RUN set -eu; \
    ldconfig; \
    mkdir /tmp/cuda-stubs; \
    ln -s /usr/local/cuda/lib64/stubs/libcuda.so /tmp/cuda-stubs/libcuda.so.1; \
    export LD_LIBRARY_PATH=/usr/local/lib:/tmp/cuda-stubs; \
    test -f "${NIXL_PLUGIN_DIR}/libplugin_POSIX.so"; \
    for binary in \
      /usr/local/bin/pagebroker \
      /usr/local/bin/pagebroker-cuda-worker \
      "${NIXL_PLUGIN_DIR}/libplugin_POSIX.so"; do \
      dependencies=$(ldd "${binary}"); \
      if printf '%s\n' "${dependencies}" | grep -q 'not found'; then \
        printf 'unresolved dependency in %s:\n%s\n' "${binary}" "${dependencies}" >&2; \
        exit 1; \
      fi; \
    done; \
    ldd /usr/local/bin/pagebroker | grep -q libnixl; \
    ldd /usr/local/bin/pagebroker-cuda-worker | grep -q libnixl; \
    /tmp/cuda-nixl-transfer-test; \
    rm -rf /tmp/cuda-nixl-transfer-test /tmp/cuda-stubs

ENTRYPOINT ["/usr/local/bin/pagebroker"]

FROM ${AGENT_BASE_IMAGE} AS pagebroker

RUN apt-get update && apt-get install -y --no-install-recommends libprotobuf32t64 libssl3t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/pagebroker/pagebroker /usr/local/bin/pagebroker
COPY --from=build /src/pagebroker/pagebroker-cuda-worker /usr/local/bin/pagebroker-cuda-worker
COPY --from=build /cuinterpose-build/cuinterpose-coordinator /usr/local/bin/cuinterpose-coordinator
ENTRYPOINT ["/usr/local/bin/pagebroker"]
