# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

FROM python:3.12-slim AS model-streamer-install

COPY model-streamer-wheel.sha256 /model-streamer-wheel.sha256
COPY --from=model-streamer-wheel / /wheels/
RUN set -eu; \
    set -- /wheels/*.whl; \
    [ "$#" -eq 1 ] && [ -f "$1" ] || { echo "exactly one Model Streamer wheel is required" >&2; exit 1; }; \
    expected=$(sed -n '1p' /model-streamer-wheel.sha256); \
    actual=$(sha256sum "$1"); actual=${actual%% *}; \
    [ "$actual" = "$expected" ] || { echo "Model Streamer wheel SHA-256 mismatch: expected $expected, got $actual" >&2; exit 1; }; \
    python3 -m pip install --no-cache-dir --no-deps --target /opt/model-streamer "$1"; \
    package=/opt/model-streamer/runai_model_streamer; \
    test -f "$package/libstreamer/lib/libstreamer.so"; \
    mkdir -p /opt/model-streamer-sdk/lib /opt/model-streamer-sdk/legal; \
    cp "$package/libstreamer/lib/libstreamer.so" /opt/model-streamer-sdk/lib/; \
    license=$(find /opt/model-streamer -path '*.dist-info/LICENSE' -print -quit); \
    metadata=$(find /opt/model-streamer -path '*.dist-info/METADATA' -print -quit); \
    test -n "$license"; \
    test -n "$metadata"; \
    cp "$license" /opt/model-streamer-sdk/legal/LICENSE; \
    cp "$metadata" /opt/model-streamer-sdk/legal/METADATA

FROM ubuntu:24.04 AS build

COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libprotobuf-dev \
    protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN make daemon

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends libprotobuf32t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/pagebroker /usr/local/bin/pagebroker
COPY --from=model-streamer-install /opt/model-streamer-sdk/lib/libstreamer.so /usr/local/lib/libstreamer.so
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/LICENSE /legal/model-streamer/LICENSE
COPY --from=model-streamer-install /opt/model-streamer-sdk/legal/METADATA /legal/model-streamer/METADATA
ENTRYPOINT ["/usr/local/bin/pagebroker"]
