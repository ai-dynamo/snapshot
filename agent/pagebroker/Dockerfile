# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG DOCKER_PROXY
ARG PAGEBROKER_BASE_IMAGE=ubuntu:24.04

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BASE_IMAGE} AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libgtest-dev \
    libprotobuf-dev \
    pkg-config \
    protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN make test && make daemon

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BASE_IMAGE}

RUN apt-get update && apt-get install -y --no-install-recommends libprotobuf32t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/pagebroker /usr/local/bin/pagebroker
RUN output=$(/usr/local/bin/pagebroker 2>&1); status=$?; \
    [ "$status" -eq 2 ]; \
    [ "$output" = "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests" ]

COPY --from=build /src /legal/source/pagebroker
RUN mkdir -p /legal/libprotobuf \
    && cp /usr/share/doc/libprotobuf32t64/copyright /legal/libprotobuf/copyright \
    && rm -rf /legal/source/pagebroker/build /legal/source/pagebroker/pagebroker /legal/source/pagebroker/v1/pagebroker.pb.*

ENTRYPOINT ["/usr/local/bin/pagebroker"]
