# SPDX-FileCopyrightText: Copyright (c) 2024-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Unified Dockerfile for snapshot-agent and placeholder images.
#
# Build targets. Two named build contexts are required for the agent: `api`
# (the agent module depends on it via `replace => ../api`) and `compliance`
# (attribution helpers shared with the operator image):
#   docker build --platform linux/amd64 --target agent \
#     --build-context api=../api --build-context compliance=../hack/compliance \
#     -t snapshot-agent:latest .
#   docker build --platform linux/amd64 --target placeholder --build-context api=../api \
#     --build-arg BASE_IMAGE=<app-image> -t placeholder:latest .
#
# Optional targets for CI:
#   docker build --target linter --build-context api=../api .   # Run linting
#   docker build --target tester --build-context api=../api .   # Run tests

# =============================================================================
# Build Arguments
# =============================================================================
ARG DOCKER_PROXY
ARG GO_VERSION=1.26.6
# Default to upstream CRIU development branch. Custom forks can override both
# args at build time, for example:
#   --build-arg CRIU_REPO=<git-remote-url>
#   --build-arg CRIU_REF=<fork-branch-or-sha>
ARG CRIU_REPO=https://github.com/checkpoint-restore/criu.git
ARG CRIU_REF=criu-dev
ARG GNU_TAR_VERSION=1.35
ARG GNU_TAR_SHA256=4d62ff37342ec7aed748535323930c7cf94acf71c3591882b26a7ea50f3edc16
ARG AGENT_BASE_IMAGE=nvcr.io/nvidia/cuda-dl-base:25.11-cuda13.0-devel-ubuntu24.04@sha256:8315e2455736c4f9b597f15c5fb4d31f834e798e0c6b66bbdbdbac491ce26bd1

# For placeholder target only - this default allows agent builds to succeed,
# but placeholder builds MUST override it with --build-arg BASE_IMAGE=<image>
ARG BASE_IMAGE=placeholder-requires-base-image-arg

# =============================================================================
# Stage: Go base - Common setup for Go builds
# =============================================================================
FROM ${DOCKER_PROXY}library/golang:${GO_VERSION} AS go-base

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN echo "Building for ${TARGETOS}/${TARGETARCH}"

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# The agent module depends on the api module via `replace => ../api`, so the api
# module must sit at /api (a sibling of /workspace) for `go mod download` and the Go build to
# resolve it. Provided via the `api` named build context.
COPY --from=api . /api/

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# =============================================================================
# Stage: Linter - Run golangci-lint
# =============================================================================
FROM go-base AS linter

RUN go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.2
RUN golangci-lint run --timeout=5m

# =============================================================================
# Stage: Tester - Run tests
# =============================================================================
FROM go-base AS tester

RUN go test ./... -v

# =============================================================================
# Stage: Builder - Build Go binaries
# =============================================================================
FROM go-base AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o /snapshot-agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o /nsrestore ./cmd/nsrestore

# Corresponding source for the Go modules compiled into the binaries above.
RUN mkdir -p /go-src && go mod vendor -o /go-src/vendor

# =============================================================================
# Stage: Restore tar builder
#
# Rootfs diffs are extracted inside the placeholder container. A dynamically
# linked tar would use that image's ELF loader and glibc, which may be older
# than the agent image's. Build GNU tar against musl so restore extraction does
# not depend on any userspace library in the placeholder image.
# =============================================================================
FROM alpine:3.23 AS tar-builder

ARG GNU_TAR_VERSION
ARG GNU_TAR_SHA256

RUN apk add --no-cache \
    attr \
    build-base \
    ca-certificates \
    curl \
    xz

WORKDIR /tmp

COPY scripts/fetch-gnu-tar.sh scripts/build-gnu-tar.sh /tmp/scripts/

RUN sh /tmp/scripts/fetch-gnu-tar.sh "${GNU_TAR_VERSION}" "${GNU_TAR_SHA256}"

WORKDIR /tmp/tar-${GNU_TAR_VERSION}

RUN sh /tmp/scripts/build-gnu-tar.sh

# =============================================================================
# Stage: CUDA checkpoint helper builder
# =============================================================================
FROM ${AGENT_BASE_IMAGE} AS cuda-helper-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

COPY cmd/cuda-checkpoint-helper/main.c ./cmd/cuda-checkpoint-helper/main.c
COPY cmd/ns-bind-mount/main.c ./cmd/ns-bind-mount/main.c

RUN gcc -O2 -Wall -Wextra -o /cuda-checkpoint-helper \
    ./cmd/cuda-checkpoint-helper/main.c \
    -I/usr/local/cuda/include \
    -L/usr/local/cuda/lib64/stubs \
    -lcuda

# ns-bind-mount bind-mounts the snapshot binaries into a target container's mount
# namespace. It needs no CUDA headers, only mount_setattr (Linux 5.12+).
RUN gcc -O2 -Wall -Wextra -o /ns-bind-mount ./cmd/ns-bind-mount/main.c

# =============================================================================
# Stage: CRIU Builder - Build CRIU with CUDA plugin
# =============================================================================
FROM ubuntu:24.04 AS criu-builder

ARG CRIU_REPO
ARG CRIU_REF

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    build-essential \
    pkg-config \
    libbsd-dev \
    libcap-dev \
    libnet1-dev \
    libnl-3-dev \
    libnl-route-3-dev \
    libprotobuf-dev \
    libprotobuf-c-dev \
    protobuf-c-compiler \
    protobuf-compiler \
    python3 \
    python3-protobuf \
    libgnutls28-dev \
    libnftables-dev \
    uuid-dev \
    && rm -rf /var/lib/apt/lists/*

RUN git init /tmp/criu \
    && cd /tmp/criu \
    && git remote add origin ${CRIU_REPO} \
    && git fetch --depth 1 origin ${CRIU_REF} \
    && git checkout FETCH_HEAD \
    && make -j$(nproc) \
    && make DESTDIR=/criu-install install-criu install-lib install-cuda_plugin

COPY plugins/inet-remap/snapshot_inet_remap.c /tmp/snapshot_inet_remap.c
RUN mkdir -p /criu-install/usr/local/lib/snapshot/criu-plugins \
    && gcc -O2 -Wall -Wextra -fPIC -shared \
      -idirafter /tmp/criu/criu/include \
      -o /criu-install/usr/local/lib/snapshot/criu-plugins/snapshot_inet_remap.so \
      /tmp/snapshot_inet_remap.c

RUN git clone https://github.com/NVIDIA/cuda-checkpoint.git /tmp/cuda-checkpoint

# CRIU source at the exact ref built from: this is a from-source build, and
# CRIU_REF may be a moving branch.
RUN tar -czf /criu-src.tar.gz -C /tmp --exclude-vcs criu

# =============================================================================
# Stage: Runtime deps - CRIU runtime packages
#
# Shared by the agent and sources stages so both see the same package set.
# =============================================================================
FROM ${AGENT_BASE_IMAGE} AS runtime-deps

ARG TARGETARCH=amd64

RUN if [ "${TARGETARCH}" != "amd64" ]; then \
      echo "ERROR: Snapshot requires x86_64 (cuda-checkpoint has no ${TARGETARCH} binary)" >&2; exit 1; \
    fi

# Install CRIU runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    libbsd0 \
    libcap2 \
    libnet1 \
    libnl-3-200 \
    libnl-route-3-200 \
    libprotobuf-c1 \
    libgnutls30t64 \
    libnftables1 \
    iproute2 \
    iptables \
    procps \
    uuid-runtime \
    tar \
    ca-certificates \
    util-linux \
    && rm -rf /var/lib/apt/lists/*

# =============================================================================
# Stage: Sources - corresponding source for everything added over the base
# =============================================================================
FROM runtime-deps AS sources

COPY compliance/base-packages.tsv /tmp/base-packages.tsv
COPY compliance/collect-sources.sh /tmp/collect-sources.sh

# Debian/Ubuntu source for the delta against the NGC base image.
RUN sh /tmp/collect-sources.sh /tmp/base-packages.tsv /sources/dpkg

# CRIU source, at the exact ref this image was built from.
COPY --from=criu-builder /criu-src.tar.gz /sources/criu/criu-src.tar.gz

# Pinned upstream source for the statically linked restore-time GNU tar.
COPY --from=tar-builder /gnu-tar-source.tar.xz /sources/gnu-tar/gnu-tar-source.tar.xz

# Go module source for the statically linked binaries.
COPY --from=builder /go-src/vendor /sources/go/vendor

COPY compliance/README.source.txt /sources/README.txt

# =============================================================================
# Stage: Agent - Final snapshot-agent image
# =============================================================================
FROM runtime-deps AS agent

# Copy CRIU from builder
COPY --from=criu-builder /criu-install/usr/local /usr/local
COPY --from=criu-builder /tmp/criu/COPYING /legal/CRIU/COPYING
RUN criu --version

# Copy CUDA checkpoint binaries
COPY --from=criu-builder /tmp/cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /usr/local/sbin/cuda-checkpoint
COPY --from=criu-builder /tmp/cuda-checkpoint/LICENSE /legal/cuda-checkpoint/LICENSE
COPY --from=cuda-helper-builder /cuda-checkpoint-helper /usr/local/bin/cuda-checkpoint-helper
# nsmount resolves this helper at /usr/local/sbin/ns-bind-mount (see
# agent/internal/nsmount/mount.go defaultBinaryPath).
COPY --from=cuda-helper-builder /ns-bind-mount /usr/local/sbin/ns-bind-mount
RUN chmod +x /usr/local/sbin/cuda-checkpoint /usr/local/bin/cuda-checkpoint-helper \
    /usr/local/sbin/ns-bind-mount

# Copy the built binaries
COPY --from=builder /snapshot-agent /usr/local/bin/snapshot-agent

# Assemble the injection bundle: bind-mounted read-only into the placeholder's
# mount namespace at /tmp/snapshot-binaries during restore (internal/nsmount).
# The placeholder ships none of this. nsrestore redirects all three lookup
# mechanisms into the mount: criu's libdir -> criu-plugins/ (criu.overrideLibDir),
# LD_LIBRARY_PATH -> lib/, and PATH -> the bundle root.
RUN mkdir -p /snapshot-binaries/lib /snapshot-binaries/criu-plugins
COPY --from=builder /nsrestore /snapshot-binaries/nsrestore
COPY --from=criu-builder /criu-install/usr/local/sbin/criu /snapshot-binaries/criu
COPY --from=criu-builder /criu-install/usr/local/lib/snapshot/criu-plugins/ /snapshot-binaries/criu-plugins/
COPY --from=criu-builder /tmp/cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /snapshot-binaries/cuda-checkpoint
COPY --from=cuda-helper-builder /cuda-checkpoint-helper /snapshot-binaries/cuda-checkpoint-helper
COPY --from=tar-builder /out/tar /snapshot-binaries/tar

# Network binaries needed during restore:
#   ip  — address/route restore (criu/net.c)
#   tar — used by runtime.ApplyRootfsDiff; statically linked and copied above
# Network lock is set to SKIP (--network-lock skip) in BuildRestoreOpts, so
# criu never forks iptables-restore/ip6tables-restore on this path and neither
# binary needs to be in the bundle. See BuildRestoreOpts for why SKIP is safe
# for restore specifically.
# ip resolves through PATH inside the namespace. ApplyRootfsDiff invokes tar by
# its explicit bundle path so it cannot fall back to the placeholder's tar.
RUN set -eu; \
    cp -L "$(command -v ip)" /snapshot-binaries/; \
    chmod +x /snapshot-binaries/criu /snapshot-binaries/nsrestore \
             /snapshot-binaries/cuda-checkpoint /snapshot-binaries/cuda-checkpoint-helper \
             /snapshot-binaries/ip /snapshot-binaries/tar

# Shared-library closure of the bundled binaries and plugins.
#
# glibc (libc, libm, libpthread, libdl, librt, libresolv) and the loader are
# deliberately excluded: the kernel reads PT_INTERP before LD_LIBRARY_PATH
# applies, so the loader always comes from the placeholder, and glibc's loader
# and libc are version-locked — pairing them across builds breaks at exec.
# Also excluded: libmvec, libutil, libanl, libnsl, libthread_db — same glibc
# version-lock; libm matches only when followed by '.' or '-', not libmvec.
# libcuda/libnvidia are excluded because they are injected by the NVIDIA
# container runtime and must match the host driver.
RUN set -eu; \
    command -v ldd >/dev/null || { echo "ERROR: ldd missing in builder" >&2; exit 1; }; \
    for f in /snapshot-binaries/* /snapshot-binaries/criu-plugins/*.so; do \
      [ -f "$f" ] || continue; \
      ldd "$f" 2>/dev/null | awk '/=> \//{print $3}'; \
    done | sort -u \
    | grep -Ev '/(ld-linux|libc|libm|libmvec|libdl|libpthread|librt|libresolv|libutil|libanl|libnsl|libthread_db|libcuda|libnvidia)[.-]' \
    | xargs -r -I{} cp -Ln {} /snapshot-binaries/lib/

# Post-condition: verify each key binary resolves cleanly against the bundle's
# lib/. The closure step above uses xargs -r which exits 0 on empty input, so
# a silent ldd failure leaves lib/ empty; asserting here catches that.
RUN set -eu; \
    for f in /snapshot-binaries/criu /snapshot-binaries/ip \
             /snapshot-binaries/criu-plugins/*.so; do \
      [ -f "$f" ] || continue; \
      if LD_LIBRARY_PATH=/snapshot-binaries/lib ldd "$f" 2>&1 | grep -q 'not found'; then \
        echo "ERROR: unresolved shared libraries in $f:" >&2; \
        LD_LIBRARY_PATH=/snapshot-binaries/lib ldd "$f" >&2; \
        exit 1; \
      fi; \
    done

COPY --from=sources /sources /legal/source
COPY --from=tar-builder /out/COPYING /legal/gnu-tar/COPYING

# Attribution generated from the delta manifest and the vendored modules.
COPY compliance/gen-attribution.sh /tmp/gen-attribution.sh
COPY --from=compliance go-licenses.sh /tmp/go-licenses.sh
RUN sh /tmp/gen-attribution.sh /legal/source/dpkg/DELTA.tsv /legal/source/go/vendor \
      /legal/THIRD-PARTY.txt /legal/source/dpkg/SKIPPED.txt /tmp/go-licenses.sh \
    && rm -f /tmp/gen-attribution.sh

# Create directories
RUN mkdir -p /checkpoints /var/run/snapshot

USER root

ENTRYPOINT ["/usr/local/bin/snapshot-agent"]

# =============================================================================
# Stage: Placeholder - Runtime-compatible restore image (requires BASE_IMAGE arg)
#
# This is the runtime image with the same default execution contract
# (entrypoint/cmd), running as root so the agent can restore into it. It ships
# NO checkpoint/restore tooling: criu, nsrestore, the cuda-checkpoint binaries,
# their shared libraries, and the criu plugins are all bind-mounted on demand
# from the agent's /snapshot-binaries bundle at restore time (internal/nsmount).
# The mount destination is created by the ns-bind-mount helper inside this
# namespace, so nothing needs pre-provisioning here.
#
# Three constraints follow from that and must hold for every BASE_IMAGE:
#   - BASE_IMAGE must be glibc-compatible with AGENT_BASE_IMAGE for the
#     dynamically linked CRIU, ip, and CUDA tools. Their library closure is
#     built from AGENT_BASE_IMAGE (ubuntu 24.04, glibc 2.39) and loaded ahead
#     of the placeholder's own libraries via LD_LIBRARY_PATH. The restore tar
#     is statically linked and does not share this constraint.
#   - The node kernel must be >= 5.12 (mount_setattr, used by ns-bind-mount).
#   - The placeholder pod must NOT set readOnlyRootFilesystem: true unless a
#     writable volume is mounted at /tmp. The agent bind-mounts the binary
#     bundle into the placeholder's /tmp at restore time; with a read-only
#     root and nothing mounted at /tmp, creating the mount point fails with
#     EROFS. An emptyDir at /tmp (either medium) makes it work again, since
#     volume mounts stay writable independently of the root filesystem.
#
# cuda-checkpoint wrapping is opt-in (--cuda-checkpoint-wrap in snapshotctl).
# When used, the source workload image must have cuda-checkpoint at the same
# path as it runs from — CRIU restores file-backed mappings by path, not via
# PATH search — and so must this placeholder image (same BASE_IMAGE).
#
# Backend entrypoints run a restore standby path when the operator
# injects DYN_SNAPSHOT_RESTORE_STANDBY=1, then sleep while the
# snapshot-agent restores into the container.
# =============================================================================
FROM ${BASE_IMAGE} AS placeholder

ARG BASE_IMAGE
ARG TARGETARCH=amd64
ENV ORIGINAL_BASE_IMAGE=${BASE_IMAGE}

USER root

RUN if [ "${TARGETARCH}" != "amd64" ]; then \
      echo "ERROR: Snapshot requires x86_64" >&2; exit 1; \
    fi

# Checkpoint volume mount point. This is a storage concern — no restore tooling
# is provisioned here; see the stage comment above.
RUN mkdir -p /checkpoints
