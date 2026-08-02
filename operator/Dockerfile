# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build context is the repository root so the api/ module (referenced via the
# replace directive in operator/go.mod) is available inside the build.
FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY api/ api/
COPY operator/ operator/
WORKDIR /workspace/operator
# CGO_ENABLED=0 keeps the binaries free of any libc linkage, so the musl-based
# builder can't produce something the glibc-based runtime below won't load.
RUN CGO_ENABLED=0 go build -o bin/manager ./cmd/manager
RUN CGO_ENABLED=0 go build -o bin/crd-installer ./cmd/crdinstaller

# NVIDIA-managed distroless base, pulled from nvcr.io. OSRB requires approved
# base containers to come from an NVIDIA-managed repository; gcr.io/distroless
# does not qualify even though distroless itself is an approved base (OSRB bugs
# 4176061 / 4489229). Same image the Dynamo operator uses
# (deploy/operator/Dockerfile in ai-dynamo/dynamo, pinned there at v4.0.3).
FROM nvcr.io/nvidia/distroless/go:v4.0.8
COPY --from=builder /workspace/operator/bin/manager /manager
COPY --from=builder /workspace/operator/bin/crd-installer /crd-installer
# The gcr.io distroless :nonroot tag set this implicitly; nvcr.io's does not.
USER 65532:65532
ENTRYPOINT ["/manager"]
