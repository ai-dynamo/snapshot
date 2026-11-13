# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build context is the repository root so the Go workspace can resolve the
# api/ and agent/ modules used by the operator.
# Tracks the toolchain pinned in go.work, the three go.mod files, hack/tools.mk,
# agent/Dockerfile, and the workflows. Move them together, never this one alone.
FROM golang:1.27-alpine@sha256:eb61c3873c97416fff0116553fff65d67a3549f72e11d9758849a689cdd8b523 AS builder
ARG GO_BUILD_TAGS
WORKDIR /workspace
COPY go.work go.work.sum ./
COPY api/ api/
COPY agent/ agent/
COPY operator/ operator/
WORKDIR /workspace/operator
# CGO_ENABLED=0 keeps the binaries free of any libc linkage, so the musl-based
# builder can't produce something the glibc-based runtime below won't load.
RUN CGO_ENABLED=0 go build -tags "${GO_BUILD_TAGS}" -o bin/manager ./cmd/manager
RUN CGO_ENABLED=0 go build -o bin/crd-installer ./cmd/crdinstaller

# The runtime image is distroless (no shell), so the source tree and the
# attribution file are assembled here and copied in ready-made.
FROM builder AS sources
RUN mkdir -p /sources/go && go work vendor -o /sources/go/vendor
COPY operator/compliance/README.source.txt /sources/README.txt
COPY operator/compliance/gen-attribution.sh /tmp/gen-attribution.sh
COPY hack/compliance/go-licenses.sh /tmp/go-licenses.sh
RUN sh /tmp/gen-attribution.sh /sources/go/vendor /legal/THIRD-PARTY.txt /tmp/go-licenses.sh

FROM nvcr.io/nvidia/distroless/go:v4.2.0@sha256:10b47941ea75fbc48c2777d6652d028c9ff92b43eaca3a50ee32ff98a9ae9015
COPY --from=builder /workspace/operator/bin/manager /manager
COPY --from=builder /workspace/operator/bin/crd-installer /crd-installer
COPY --from=sources /sources /legal/source
COPY --from=sources /legal/THIRD-PARTY.txt /legal/THIRD-PARTY.txt
# The gcr.io distroless :nonroot tag set this implicitly; nvcr.io's does not.
USER 65532:65532
ENTRYPOINT ["/manager"]
