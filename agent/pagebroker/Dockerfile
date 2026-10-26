# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG DOCKER_PROXY
ARG TARGETARCH
ARG PAGEBROKER_BUILD_IMAGE=ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254
# When bumping the base, update the source URL in compliance/README.source.txt
# to the matching version.
ARG PAGEBROKER_BASE_IMAGE=nvcr.io/nvidia/distroless/cc:v4.0.8@sha256:92a46f84206c3e4610043eaef50946451019e3c479b71d280015919ba311babb

ARG CUDA_HEADERS_IMAGE=nvcr.io/nvidia/cuda-dl-base:26.08-cuda13.4-devel-ubuntu24.04@sha256:843a5e81ed49ad2a2787aa10e48e926dc1f9440522a122ec2a637c27039fc77d

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
FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS model-streamer-amd64
ARG MODEL_STREAMER_ARCH=x86_64

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS model-streamer-arm64
ARG MODEL_STREAMER_ARCH=aarch64

# Docker and the SDK use different architecture names.
FROM model-streamer-${TARGETARCH} AS model-streamer-install
ARG TARGETARCH
ARG MODEL_STREAMER_VERSION=0.17.0
ARG MODEL_STREAMER_REF=dd9e5d34f966cc2667c9f0505262050b4bedc35a
WORKDIR /opt/model-streamer-sdk
COPY model-streamer-sdk.${TARGETARCH}.sha256 /tmp/model-streamer-sdk.sha256
ADD https://github.com/dsx-ai-factory/model-streamer/releases/download/${MODEL_STREAMER_VERSION}/runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-${MODEL_STREAMER_ARCH}.tar.gz ./sdk.tar.gz
RUN sha256sum -c /tmp/model-streamer-sdk.sha256 \
    && tar -xzf sdk.tar.gz --strip-components=1 \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-${MODEL_STREAMER_ARCH}/include \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-${MODEL_STREAMER_ARCH}/lib/libstreamer.so \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-${MODEL_STREAMER_ARCH}/lib/libstreamers3.so \
      runai-model-streamer-cpp-sdk-${MODEL_STREAMER_VERSION}-linux-${MODEL_STREAMER_ARCH}/LICENSE \
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

# Download an unmodified upstream executable. The Go stage collects notices
# from the matching release's module graph; it does not rebuild or patch rclone.
FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS rclone-amd64
ARG RCLONE_SHA256=349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702

FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS rclone-arm64
ARG RCLONE_SHA256=7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10

FROM rclone-${TARGETARCH} AS rclone-install
ARG TARGETARCH
ARG PAGEBROKER_RCLONE_VERSION=1.75.2
ADD --checksum=sha256:${RCLONE_SHA256} https://downloads.rclone.org/v${PAGEBROKER_RCLONE_VERSION}/rclone-v${PAGEBROKER_RCLONE_VERSION}-linux-${TARGETARCH}.zip /tmp/rclone.zip
ADD --checksum=sha256:68afd7f68c84bd978966feae2116339aa7bf454b39c573910c462e87c3774d5a https://codeload.github.com/rclone/rclone/tar.gz/refs/tags/v${PAGEBROKER_RCLONE_VERSION} /tmp/rclone.tar.gz
RUN apt-get update && apt-get install -y --no-install-recommends unzip \
    && rm -rf /var/lib/apt/lists/* \
    && unzip -q /tmp/rclone.zip -d /tmp \
    && mkdir -p /rclone/bin /rclone/legal /src/rclone \
    && cp /tmp/rclone-v${PAGEBROKER_RCLONE_VERSION}-linux-${TARGETARCH}/rclone /rclone/bin/ \
    && tar -xzf /tmp/rclone.tar.gz --strip-components=1 -C /src/rclone \
    && cp /tmp/rclone.tar.gz /rclone/legal/source.tar.gz \
    && cp /src/rclone/COPYING /rclone/legal/LICENSE \
    && cp /usr/local/go/LICENSE /rclone/legal/Go-LICENSE \
    && /rclone/bin/rclone version > /rclone/legal/VERSION \
    && go version -m /rclone/bin/rclone > /rclone/legal/BUILD-INFO \
    && sha256sum /tmp/rclone.zip /tmp/rclone.tar.gz > /rclone/legal/SHA256SUMS
WORKDIR /src/rclone
RUN go mod download all \
    && find /go/pkg/mod -type f \( -iname 'license*' -o -iname 'notice*' -o -iname 'copying*' \) \
       ! -path '/go/pkg/mod/cache/*' | sort > /tmp/notices \
    && while IFS= read -r notice; do \
         printf '\nCOMPONENT NOTICE: %s\n' "${notice#/go/pkg/mod/}" && cat "$notice" || exit 1; \
       done < /tmp/notices > /rclone/legal/THIRD-PARTY-NOTICES.txt

# Native developers can export the same executable and attribution.
FROM scratch AS rclone-artifacts
COPY --from=rclone-install /rclone/ /

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS build-dependencies
COPY --from=rclone-install /rclone/bin/rclone /usr/local/bin/rclone

COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/include/ /usr/local/include/
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamers3.so /usr/local/lib/libstreamers3.so

WORKDIR /src
COPY compliance/ compliance/
# Fetch sources in the install layer so a cached install never pairs with a
# newer archive index. Ubuntu drops superseded versions from the index, so a
# later layer cannot fetch the source of the version installed here.
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    libaio-dev \
    libgtest-dev \
    libprotobuf-dev \
    libssl-dev \
    nlohmann-json3-dev=3.11.3-1 \
    pkg-config \
    protobuf-compiler \
    && sh compliance/collect-sources.sh /legal/source/protobuf libprotobuf-dev \
    && sh compliance/collect-sources.sh /legal/source/libaio libaio1t64 \
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
# Run unit tests unprivileged so directory permissions are enforced.
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

RUN arch="$(uname -m)" \
    && mv /usr/local/lib /usr/local/lib-build \
    && mkdir -p /runtime-lib \
    && cp -a "/usr/lib/${arch}-linux-gnu"/libaio.so.* /runtime-lib/ \
    && for library in /nixl-runtime-check/libnixl*.so /nixl-runtime-check/plugins/libplugin_POSIX.so; do \
         LD_LIBRARY_PATH=/nixl-runtime-check:/runtime-lib ldd "$library" > /tmp/dependencies \
         && ! grep -q 'not found' /tmp/dependencies || { cat /tmp/dependencies; exit 1; }; \
       done \
    && mv /usr/local/lib-build /usr/local/lib

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
COPY --from=rclone-install /rclone/bin/rclone /usr/local/bin/rclone
COPY --from=rclone-install /rclone/legal/ /legal/rclone/
COPY --from=build /usr/share/doc/nlohmann-json3-dev/copyright /legal/nlohmann-json/copyright
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/ /legal/model-streamer/
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamers3.so /usr/local/lib/libstreamers3.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/LICENSE /legal/model-streamer-s3/LICENSE

# libstreamers3.so is loaded with dlopen inside libstreamer.so; the executable's
# RUNPATH alone does not cover that lookup or all transitive dependencies.

ENTRYPOINT ["/usr/local/bin/pagebroker"]

FROM runtime AS final
