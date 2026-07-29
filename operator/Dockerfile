# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build context is the repository root so the api/ module (referenced via the
# replace directive in operator/go.mod) is available inside the build.
FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY api/ api/
COPY operator/ operator/
WORKDIR /workspace/operator
RUN go build -o bin/manager ./cmd/manager
# The CRD installer ships in the same image; the chart runs it as the operator's
# init container by overriding the entrypoint.
RUN go build -o bin/crd-installer ./cmd/crdinstaller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/operator/bin/manager /manager
COPY --from=builder /workspace/operator/bin/crd-installer /crd-installer
# The CRDs controller-gen generates from api/, carried at the path crd-installer
# reads by default. Baking them in ties the definitions to the binary compiled
# against the same types, so an image can never apply a schema it disagrees with.
COPY charts/snapshot/crds/ /etc/snapshot/crds/
ENTRYPOINT ["/manager"]
