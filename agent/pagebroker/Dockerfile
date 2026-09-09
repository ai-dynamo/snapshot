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
# stage and pinned to the source version of the libprotobuf-dev that provided
# the libprotobuf.a linked above, so the shipped source cannot be a different
# version than the linked object.
RUN for f in /etc/apt/sources.list.d/*.sources; do \
      [ -f "$f" ] && sed -i 's/^Types: deb$/Types: deb deb-src/' "$f" || true; \
    done \
    && { [ ! -f /etc/apt/sources.list ] \
         || sed -i 's/^deb \(.*\)$/deb \1\ndeb-src \1/' /etc/apt/sources.list; } \
    && apt-get update \
    && mkdir -p /legal/source/protobuf \
    && version="$(dpkg-query -W -f='${source:Version}' libprotobuf-dev)" \
    && (cd /legal/source/protobuf \
        && apt-get source --only-source --download-only "protobuf=${version}") \
    && printf '%s\n' "${version}" > /legal/source/protobuf/VERSION \
    && cp /usr/share/doc/libprotobuf-dev/copyright /legal/source/protobuf/copyright \
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

RUN { \
      printf '================================================================================\n'; \
      printf 'THIRD-PARTY SOFTWARE NOTICES AND ATTRIBUTION\n'; \
      printf 'NVIDIA Dynamo Snapshot (PageBroker image)\n'; \
      printf '================================================================================\n\n'; \
      printf 'This image statically links protobuf. Corresponding source ships under\n'; \
      printf '/legal/source/protobuf/.\n\n'; \
      printf '================================================================================\n'; \
      printf 'COMPONENT: protobuf\n'; \
      printf '================================================================================\n'; \
      cat /legal/source/protobuf/copyright; \
    } > /legal/THIRD-PARTY.txt

FROM ${PAGEBROKER_BASE_IMAGE}

COPY --from=build /src/pagebroker /usr/local/bin/pagebroker
COPY --from=sources /legal /legal

ENTRYPOINT ["/usr/local/bin/pagebroker"]
