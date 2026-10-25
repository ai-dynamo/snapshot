# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build these images from the same checkout with the production Dockerfile.
ARG BUILD_IMAGE
ARG RUNTIME_IMAGE
FROM ${BUILD_IMAGE} AS tests
USER root
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv openssl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --user-group integration
WORKDIR /src/agent/pagebroker
COPY --chown=integration:integration agent/pagebroker/ /src/agent/pagebroker/
COPY --chown=integration:integration hack/pagebroker-integration/ /src/hack/pagebroker-integration/
RUN chown -R integration:integration /src
USER integration
RUN make -f Makefile -f ../../hack/pagebroker-integration/tests.mk integration-test CUDA_ROOT=/opt/cuda

# Run the same S3 checks against the production runtime and its trust store.
FROM ${RUNTIME_IMAGE} AS s3-test
COPY --from=tests /src/agent/pagebroker/build/model-streamer-s3-test /usr/local/bin/model-streamer-s3-test
ENTRYPOINT ["/usr/local/bin/model-streamer-s3-test"]
