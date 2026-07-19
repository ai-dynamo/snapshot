# Copyright 2026 NVIDIA Corporation
# SPDX-License-Identifier: Apache-2.0

# Build context is the repository root so the api/ module (referenced via the
# replace directive in operator/go.mod) is available inside the build.
FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY api/ api/
COPY operator/ operator/
WORKDIR /workspace/operator
RUN go build -o bin/manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/operator/bin/manager /manager
ENTRYPOINT ["/manager"]
