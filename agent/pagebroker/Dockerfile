# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG DOCKER_PROXY
ARG PAGEBROKER_BUILD_IMAGE=ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
# When bumping the base, update the source URL in compliance/README.source.txt
# to the matching version.
ARG PAGEBROKER_BASE_IMAGE=nvcr.io/nvidia/distroless/cc:v4.0.8@sha256:8003f23a7122e630e3a78875dda6e68403c7ccc0adb393470a8c7739fafa4a02

FROM ${DOCKER_PROXY}library/${PAGEBROKER_BUILD_IMAGE} AS build

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

# Smoke-test must run here: the runtime stage is distroless (no shell).
RUN output=$(/src/pagebroker 2>&1); status=$?; \
    [ "$status" -eq 2 ]; \
    [ "$output" = "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests" ]

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
COPY --from=sources /legal /legal

ENTRYPOINT ["/usr/local/bin/pagebroker"]
