# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The upstream release does not yet contain this C API. Build the wheel and
# public headers together; the toolchain and source are both immutable inputs.
FROM ghcr.io/dsx-ai-factory/model-streamer/devcontainer:latest@sha256:2cbec192e8c690893381a5c5bb79836b0d6200b7b6e2d4884daab85492d232a7 AS build
WORKDIR /src
RUN git init . \
    && git remote add origin https://github.com/dsx-ai-factory/model-streamer.git \
    && git fetch --depth 1 origin ae93548e02be46f479e7689a7e10e9bb243bb653 \
    && git checkout --detach FETCH_HEAD
# SOURCE_DATE_EPOCH fixes the ZIP timestamps; no wall-clock data enters the wheel.
RUN export SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)" \
    && export PACKAGE_VERSION=0.0.0+git.ae93548 \
    && make ci-build COMPONENT=streamer ARCH=x86_64
RUN mkdir -p /out/core /out/include/streamer /out/legal \
    && cp py/runai_model_streamer/dist/*x86_64.whl /out/core/ \
    && cp cpp/streamer/api/streamer/*.h /out/include/streamer/ \
    && cp LICENSE /out/legal/LICENSE \
    && git rev-parse HEAD > /out/legal/REVISION

# Preserve the sources of the two I/O libraries statically linked by this
# toolchain alongside the Streamer source and its public headers.
RUN mkdir -p /out/legal/source \
    && git archive HEAD | gzip -n > /out/legal/source/model-streamer.tar.gz
RUN curl -fsSL -o /out/legal/source/libaio.tar.gz https://releases.pagure.org/libaio/libaio-0.3.113.tar.gz \
    && echo "2c44d1c5fd0d43752287c9ae1eb9c023f04ef848ea8d4aafa46e9aedb678200b  /out/legal/source/libaio.tar.gz" | sha256sum -c - \
    && curl -fsSL -o /out/legal/source/liburing.tar.gz https://github.com/axboe/liburing/archive/refs/tags/liburing-2.14.tar.gz \
    && echo "5f80964108981c6ad979c735f0b4877d5f49914c2a062f8e88282f26bf61de0c  /out/legal/source/liburing.tar.gz" | sha256sum -c -

FROM scratch AS artifacts
COPY --from=build /out/ /
