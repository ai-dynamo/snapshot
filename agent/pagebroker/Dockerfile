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
# POSIX-only NIXL v1.5.0, including upstream Linux AIO failure recovery.
ARG NIXL_REF=1683cf3b7f3d11674c03c5e861cea22339876c96
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
    && cp /nixl-install/usr/local/lib/plugins/libplugin_POSIX.so /nixl-runtime/lib/plugins/ \
    && cp subprojects/liburing-liburing-2.14/LICENSE /nixl-runtime/liburing-LICENSE \
    && printf '%s\n' "${NIXL_REF}" > /nixl-runtime/NIXL_COMMIT \
    && tar -czf /nixl-src.tar.gz -C /tmp --exclude-vcs --exclude=nixl/build nixl
# NIXL also links bundled dependencies such as Abseil. Ship their notices
# from the exact source tree used above, alongside the complete source archive.
RUN find /tmp/nixl/subprojects -type f \( -iname 'license*' -o -iname 'copying*' \) \
      ! -path '*/build/*' > /tmp/nixl-notices \
    && sort -o /tmp/nixl-notices /tmp/nixl-notices \
    && while IFS= read -r notice; do \
        printf '\nCOMPONENT NOTICE: %s\n' "${notice#/tmp/nixl/}" && cat "$notice" || exit 1; \
      done < /tmp/nixl-notices > /nixl-runtime/THIRD-PARTY-NOTICES.txt
RUN git -C /tmp/nvtx rev-parse HEAD > /NVTX_COMMIT

# Download the release SDK inside the PageBroker build so every image builder
# uses the same checksummed headers and library without preparing local inputs.
FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS model-streamer-install
ARG MODEL_STREAMER_VERSION=0.17.0
ARG MODEL_STREAMER_REF=dd9e5d34f966cc2667c9f0505262050b4bedc35a
WORKDIR /opt/model-streamer-sdk
COPY model-streamer-sdk.sha256 /tmp/model-streamer-sdk.sha256
ADD https://github.com/dsx-ai-factory/model-streamer/releases/download/${MODEL_STREAMER_VERSION}/runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-x86_64.tar.gz ./sdk.tar.gz
RUN sha256sum -c /tmp/model-streamer-sdk.sha256 \
    && tar -xzf sdk.tar.gz --strip-components=1 \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-x86_64/include \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-x86_64/lib/libstreamer.so \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-x86_64/lib/libstreamers3.so \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-x86_64/LICENSE \
    && rm sdk.tar.gz \
    && mkdir -p legal \
    && mv LICENSE legal/LICENSE \
    && cp /tmp/model-streamer-sdk.sha256 legal/SDK.sha256 \
    && printf '%s\n' "${MODEL_STREAMER_VERSION}" > legal/VERSION \
    && printf '%s\n' "${MODEL_STREAMER_REF}" > legal/REVISION \
    && printf 'Release: https://github.com/dsx-ai-factory/model-streamer/releases/tag/%s\nBuild: https://github.com/dsx-ai-factory/model-streamer/blob/%s/.devcontainer/Dockerfile\n' \
      "${MODEL_STREAMER_VERSION}" "${MODEL_STREAMER_REF}" > legal/UPSTREAM.txt

# Preserve corresponding sources for the core library and its statically linked
# I/O dependencies. These archives are attribution inputs, not build inputs.
# Dependency versions and checksums follow Model Streamer's pinned build (see
# legal/UPSTREAM.txt); changing these archives does not change the prebuilt SDK.
ADD --checksum=sha256:c344e2ee677a3c3e4cb65fc7ab1e04918d85c2f7268e9d4465a895cea2082499 https://codeload.github.com/dsx-ai-factory/model-streamer/tar.gz/${MODEL_STREAMER_REF} /opt/model-streamer-sdk/legal/source/model-streamer.tar.gz
ADD --checksum=sha256:2c44d1c5fd0d43752287c9ae1eb9c023f04ef848ea8d4aafa46e9aedb678200b https://deb.debian.org/debian/pool/main/liba/libaio/libaio_0.3.113.orig.tar.gz /opt/model-streamer-sdk/legal/source/libaio.tar.gz
ADD --checksum=sha256:5f80964108981c6ad979c735f0b4877d5f49914c2a062f8e88282f26bf61de0c https://github.com/axboe/liburing/archive/refs/tags/liburing-2.14.tar.gz /opt/model-streamer-sdk/legal/source/liburing.tar.gz

# Native builds can export the same SDK without building the daemon image.
FROM scratch AS model-streamer-sdk
COPY --from=model-streamer-install /opt/model-streamer-sdk/ /

# The S3 client has its own SDK; the Model Streamer plugin's SDK symbols are
# private. Pin the source and its submodules rather than a host-installed SDK.
FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS aws-sdk
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates cmake git libssl-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
RUN git init /src/aws-sdk-cpp \
    && cd /src/aws-sdk-cpp \
    && git remote add origin https://github.com/aws/aws-sdk-cpp.git \
    && git fetch --depth 1 origin e03a5aa84e101193f10ef8b40ba14951f5ace625 \
    && git checkout --detach FETCH_HEAD \
    && git submodule update --init --recursive --depth 1
RUN cmake -S /src/aws-sdk-cpp -B /src/aws-sdk-cpp/build -DCMAKE_BUILD_TYPE=Release \
       -DCMAKE_INSTALL_PREFIX=/opt/aws-sdk -DCMAKE_INSTALL_LIBDIR=lib \
       -DBUILD_ONLY=s3-crt -DUSE_CRT_HTTP_CLIENT=ON -DBUILD_SHARED_LIBS=ON \
       -DENABLE_TESTING=OFF -DAUTORUN_UNIT_TESTS=OFF \
    && cmake --build /src/aws-sdk-cpp/build --parallel 4 \
    && cmake --install /src/aws-sdk-cpp/build

# Include the HTTP/TLS dependency closure in the shell-free runtime. The base
# already supplies the C/C++ runtime. Keep package attribution with each copy.
RUN set -eu; \
    mkdir -p /runtime/lib /runtime/legal/aws-sdk /runtime/legal/packages; \
    cp -a /opt/aws-sdk/lib/*.so* /runtime/lib/; \
    cp /src/aws-sdk-cpp/LICENSE /runtime/legal/aws-sdk/; \
    git -C /src/aws-sdk-cpp rev-parse HEAD > /runtime/legal/aws-sdk/REVISION; \
    find /src/aws-sdk-cpp/crt -name LICENSE -o -name NOTICE | while read -r notice; do \
      relative=${notice#/src/aws-sdk-cpp/}; \
      mkdir -p "/runtime/legal/aws-sdk/$(dirname "$relative")"; \
      cp "$notice" "/runtime/legal/aws-sdk/$relative"; \
    done; \
    LD_LIBRARY_PATH=/opt/aws-sdk/lib ldd /opt/aws-sdk/lib/*.so > /tmp/aws-ldd; \
    if grep -q 'not found' /tmp/aws-ldd; then cat /tmp/aws-ldd; exit 1; fi; \
    awk '$2 == "=>" && substr($3, 1, 1) == "/" { print $3 }' /tmp/aws-ldd | sort -u > /tmp/aws-libraries; \
    while read -r library; do \
      case "$library" in \
        */libc.so.*|*/libm.so.*|*/libstdc++.so.*|*/libgcc_s.so.*|*/libpthread.so.*|*/libdl.so.*|*/librt.so.*) continue ;; \
        /opt/aws-sdk/*) continue ;; \
      esac; \
      cp -L "$library" /runtime/lib/; \
      package=$(dpkg-query -S "$(readlink -f "$library")" | head -1 | cut -d: -f1); \
      mkdir -p "/runtime/legal/packages/$package"; \
      cp "/usr/share/doc/$package/copyright" "/runtime/legal/packages/$package/"; \
    done < /tmp/aws-libraries

FROM aws-sdk AS build-dependencies

COPY --from=aws-sdk /opt/aws-sdk /usr/local
RUN rm -rf /src/aws-sdk-cpp && ldconfig

COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/include/ /usr/local/include/
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamers3.so /usr/local/lib/libstreamers3.so

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libaio-dev \
    libgtest-dev \
    libprotobuf-dev \
    libssl-dev \
    openssl \
    pkg-config \
    protobuf-compiler \
    python3 \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

COPY --from=cuda-headers /usr/local/cuda/include/ /opt/cuda/include/
COPY --from=cuda-headers /usr/local/cuda/lib64/stubs/ /opt/cuda/lib64/stubs/
COPY --from=nixl /nixl-install/usr/local/ /usr/local/
COPY --from=nixl /nixl-runtime/lib/ /nixl-runtime-check/
ENV LD_LIBRARY_PATH=/usr/local/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
ENV NIXL_PLUGIN_DIR=/usr/local/lib/plugins

FROM build-dependencies AS build
WORKDIR /src
RUN useradd --create-home --user-group pagebroker-test \
    && chown pagebroker-test:pagebroker-test /src
COPY --chown=pagebroker-test:pagebroker-test . .
USER pagebroker-test
# Run CPU and local S3 tests unprivileged so directory permissions are enforced.
# GPU tests are compiled here and run separately.
RUN test "$(id -u)" -ne 0 \
    && make test CUDA_ROOT=/opt/cuda NIXL_ROOT=/usr/local \
    && make build/gpu-checkpoint-test build/gpu-transfer-test CUDA_ROOT=/opt/cuda CUDA_LIBRARY_DIR=/opt/cuda/lib64/stubs \
    && make daemon CUDA_ROOT=/opt/cuda NIXL_ROOT=/usr/local

# Packaging below needs root to collect libraries and corresponding sources.
USER root

# The CLI remains usable without a driver. CUDA is loaded only for capability
# detection, and no build-time driver stub is shipped in the runtime image.
RUN /src/pagebroker 2>/tmp/usage; status=$?; [ "$status" -eq 2 ] && grep -q '^usage: pagebroker ' /tmp/usage

RUN mv /usr/local/lib /usr/local/lib-build \
    && mkdir -p /runtime-lib \
    && cp -a /usr/lib/x86_64-linux-gnu/libaio.so.* /runtime-lib/ \
    && for library in /nixl-runtime-check/libnixl*.so /nixl-runtime-check/plugins/libplugin_POSIX.so; do \
         LD_LIBRARY_PATH=/nixl-runtime-check:/runtime-lib ldd "$library" > /tmp/dependencies \
         && ! grep -q 'not found' /tmp/dependencies || { cat /tmp/dependencies; exit 1; }; \
       done \
    && mv /usr/local/lib-build /usr/local/lib

# Keep source and attribution aligned with the exact libraries being shipped.
RUN sh compliance/collect-sources.sh /legal/source/protobuf libprotobuf-dev \
    && sh compliance/collect-sources.sh /legal/source/libaio libaio1t64 \
    && rm -rf /var/lib/apt/lists/*

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS sources
COPY --from=build /legal/source /legal/source
COPY --from=build /src /legal/source/pagebroker
COPY --from=nixl /tmp/nvtx/c /legal/source/nvtx/c
COPY --from=nixl /tmp/nvtx/LICENSE.txt /legal/source/nvtx/copyright
COPY --from=nixl /NVTX_COMMIT /legal/source/nvtx/VERSION
COPY --from=nixl /nixl-src.tar.gz /legal/source/nixl/nixl-src.tar.gz
COPY --from=nixl /nixl-runtime/NIXL_COMMIT /legal/source/nixl/VERSION
COPY --from=nixl /tmp/nixl/LICENSE /legal/source/nixl/copyright
COPY --from=nixl /nixl-runtime/liburing-LICENSE /legal/source/nixl/liburing-LICENSE
COPY --from=nixl /nixl-runtime/THIRD-PARTY-NOTICES.txt /legal/source/nixl/THIRD-PARTY-NOTICES.txt
COPY compliance/README.source.txt /legal/source/README.txt
RUN rm -rf /legal/source/pagebroker/build /legal/source/pagebroker/pagebroker \
           /legal/source/pagebroker/v1/pagebroker.pb.*
COPY compliance/gen-attribution.sh /tmp/gen-attribution.sh
RUN sh /tmp/gen-attribution.sh /legal/source/protobuf /legal/THIRD-PARTY.txt \
    /legal/source/libaio /legal/source/nixl /legal/source/nvtx

FROM ${PAGEBROKER_BASE_IMAGE} AS runtime

COPY --from=build /src/pagebroker /usr/local/bin/pagebroker
COPY --from=build /runtime-lib/ /usr/local/lib/
COPY --from=nixl /nixl-runtime/lib/ /usr/local/lib/
COPY --from=sources /legal /legal
ENV LD_LIBRARY_PATH=/usr/local/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
ENV NIXL_PLUGIN_DIR=/usr/local/lib/plugins
COPY --from=build /usr/lib/x86_64-linux-gnu/libcrypto.so.3 /usr/local/lib/libcrypto.so.3
COPY --from=build /usr/share/doc/libssl-dev/copyright /legal/openssl/copyright
COPY --from=aws-sdk /runtime/lib/ /usr/local/lib/
COPY --from=aws-sdk /runtime/legal/ /legal/
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/ /legal/model-streamer/
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamers3.so /usr/local/lib/libstreamers3.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/LICENSE /legal/model-streamer-s3/LICENSE

# libstreamers3.so is loaded with dlopen inside libstreamer.so; the executable's
# RUNPATH alone does not cover that lookup or all transitive dependencies.

ENTRYPOINT ["/usr/local/bin/pagebroker"]

# Exercise real S3 reads with the exact production runtime and trust store.
FROM runtime AS s3-test
COPY --from=build /src/build/model-streamer-s3-test /usr/local/bin/model-streamer-s3-test
ENTRYPOINT ["/usr/local/bin/model-streamer-s3-test"]

FROM runtime AS final
