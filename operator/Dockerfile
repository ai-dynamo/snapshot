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

FROM nvcr.io/nvidia/distroless/go:v4.0.8@sha256:938bd28ce4ddb800118a951774203e7563c2067a72b20ae6b85d2c7da165a178
COPY --from=builder /workspace/operator/bin/manager /manager
COPY --from=builder /workspace/operator/bin/crd-installer /crd-installer
# The gcr.io distroless :nonroot tag set this implicitly; nvcr.io's does not.
USER 65532:65532
ENTRYPOINT ["/manager"]
