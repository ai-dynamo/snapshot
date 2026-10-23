# SPDX-FileCopyrightText: Copyright (c) 2024-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Dockerfile for the snapshot-agent image.
#
# Build targets. Two named build contexts are required for the agent: `api`
# (the agent module depends on it via `replace => ../api`) and `compliance`
# (attribution helpers shared with the operator image):
#   docker build --platform linux/amd64 --target agent \   (or linux/arm64)
#     --build-context api=../api --build-context compliance=../hack/compliance \
#     -t snapshot-agent:latest .
#
# Optional targets for CI:
#   docker build --target linter --build-context api=../api .   # Run linting
#   docker build --target tester --build-context api=../api .   # Run tests

# =============================================================================
# Build Arguments
# =============================================================================
ARG DOCKER_PROXY
ARG GO_VERSION=1.27.3
# Digest of golang:${GO_VERSION}. A digest is content-addressed, so it resolves
# identically through DOCKER_PROXY pull-through mirrors. Docker resolves
# tag@digest by digest and never checks the tag against it, so the two must be
# updated together — the go-base stage asserts they agree rather than trusting
# the tag.
ARG GO_IMAGE_DIGEST=sha256:6928d94d1142af58816fed5a62338d605a266ccd53a0deaab6b1096f19059261
# Keep in step with GOLANGCI_LINT_VERSION in hack/tools.mk. Used by the linter
# stage, which is a developer target (`--target linter`) and so is not covered
# by the CI `check` job.
ARG GOLANGCI_LINT_VERSION=v2.14.0
# Default to upstream CRIU development branch. Custom forks can override both
# args at build time, for example:
#   --build-arg CRIU_REPO=<git-remote-url>
#   --build-arg CRIU_REF=<fork-branch-or-sha>
ARG CRIU_REPO=https://github.com/checkpoint-restore/criu.git
ARG CRIU_REF=criu-dev
ARG CUDA_HEADERS_IMAGE=nvcr.io/nvidia/cuda-dl-base:26.08-cuda13.4-devel-ubuntu24.04@sha256:843a5e81ed49ad2a2787aa10e48e926dc1f9440522a122ec2a637c27039fc77d
ARG AGENT_BASE_IMAGE=nvcr.io/nvidia/cuda-dl-base:25.11-cuda13.0-devel-ubuntu24.04@sha256:8315e2455736c4f9b597f15c5fb4d31f834e798e0c6b66bbdbdbac491ce26bd1

# =============================================================================
# Stage: Go base - Common setup for Go builds
# =============================================================================
FROM ${DOCKER_PROXY}library/golang:${GO_VERSION}@${GO_IMAGE_DIGEST} AS go-base

ARG TARGETOS=linux
ARG TARGETARCH
ARG GO_VERSION

RUN echo "Building for ${TARGETOS}/${TARGETARCH}"

# Docker resolves image:tag@digest by digest alone and never validates the tag
# against it. Overriding GO_VERSION without the matching GO_IMAGE_DIGEST would
# otherwise build on the digest's toolchain while every label claims the new
# one. Assert instead of trusting the tag.
RUN actual="$(go env GOVERSION)"; \
    [ "${actual}" = "go${GO_VERSION}" ] || { \
      echo "ERROR: GO_VERSION=${GO_VERSION} but the pinned GO_IMAGE_DIGEST provides ${actual}." >&2; \
      echo "       Update GO_VERSION and GO_IMAGE_DIGEST in agent/Dockerfile together." >&2; \
      exit 1; \
    }

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

ARG GO_VERSION
ARG GOLANGCI_LINT_VERSION

# Same constraint as hack/tools.mk: golangci-lint refuses to run when it was
# built with an older Go than the module it lints, and its own go.mod pins a
# toolchain that lags ours — so force ours.
RUN GOTOOLCHAIN=go${GO_VERSION} \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}
RUN golangci-lint run --timeout=5m

# =============================================================================
# Stage: Tester - Run tests
# =============================================================================
FROM go-base AS tester

RUN CGO_ENABLED=0 go test ./... -v

# =============================================================================
# Stage: Builder - Build Go binaries
# =============================================================================
FROM go-base AS builder

ARG TARGETOS=linux
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o /snapshot-agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o /nsrestore ./cmd/nsrestore
# Static: it runs as PID 1 inside the user's restore image, which may not
# carry a compatible libc or loader.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o /snapshot-restore-proxy ./cmd/snapshot-restore-proxy

# Corresponding source for the Go modules compiled into the binaries above.
RUN mkdir -p /go-src && go mod vendor -o /go-src/vendor

# =============================================================================
# Stage: Conventional CUDA checkpoint helper builder
# =============================================================================
FROM ${CUDA_HEADERS_IMAGE} AS cuda-headers
FROM ${AGENT_BASE_IMAGE} AS cuda-helper-builder

RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
COPY --from=cuda-headers /usr/local/cuda/include/ /opt/cuda/include/
COPY --from=cuda-headers /usr/local/cuda/lib64/stubs/ /opt/cuda/lib64/stubs/
WORKDIR /workspace
COPY cmd/cuda-checkpoint-helper/ cmd/cuda-checkpoint-helper/
COPY cmd/ns-bind-mount/main.c cmd/ns-bind-mount/main.c
RUN make -C cmd/cuda-checkpoint-helper helper CUDA_ROOT=/opt/cuda \
    && cp cmd/cuda-checkpoint-helper/bin/cuda-checkpoint-helper /cuda-checkpoint-helper

# The mount helper needs no CUDA headers, only mount_setattr (Linux 5.12+).
RUN gcc -O2 -Wall -Wextra -o /ns-bind-mount cmd/ns-bind-mount/main.c

# =============================================================================
# Stage: cuInterpose shim, coordinator, and launcher
# =============================================================================
# C frontend and Rust core. Pinned GNU builder and static musl tools.
# GCC 15 builds the C frontend, which requires musttail. The Bookworm image supplies
# glibc 2.36.
FROM gcc:15.2.0-bookworm@sha256:9ca91b05c7b07d2979f16413e8b2cd6ec8a7c80ffca4121ccab0aeba33f90460 AS gcc-15

FROM rust:1.95.0-slim-bookworm@sha256:d7482085ff5b415f84dba5647ae71606650bdef00db7aeb69f4b3d170c3e4082 AS cuinterpose-builder
COPY --from=cuda-headers /usr/local/cuda/include/cuda.h /opt/cuda/include/cuda.h
COPY --from=gcc-15 /usr/local/bin/ /opt/gcc-15/bin/
COPY --from=gcc-15 /usr/local/lib/gcc/ /opt/gcc-15/lib/gcc/
COPY --from=gcc-15 /usr/local/libexec/gcc/ /opt/gcc-15/libexec/gcc/
COPY --from=gcc-15 /usr/local/lib64/ /opt/gcc-15/lib64/
RUN arch="$(uname -m)" \
    && test "$(rustc --version | cut -d' ' -f2)" = 1.95.0 \
    && apt-get update && apt-get install -y --no-install-recommends \
       musl-tools make gcc binutils \
    && rm -rf /var/lib/apt/lists/* \
    && cargo install cbindgen --version 0.29.4 --locked \
    && rustup target add "${arch}-unknown-linux-musl" \
    && rustup component add rust-src rustfmt clippy
WORKDIR /cuinterpose
COPY cmd/cuinterpose/ ./
# make frontend checks the cbindgen CLI against the locked ABI dev-dependency.
RUN make native
# Keep matching dependency and standard-library sources as part of the image's
# source bundle. This is a packaging policy, not a special Rust source requirement.
RUN mkdir -p /rust-sources && cd rust && cargo vendor --locked /rust-sources/vendor \
    && cp Cargo.lock /rust-sources/ \
    && rustc --version > /rust-sources/toolchain-version.txt \
    && cp -a "$(rustc --print sysroot)/lib/rustlib/src/rust" /rust-sources/standard-library \
    && cp -a "$(rustc --print sysroot)/share/doc/rust" /rust-sources/toolchain-notices

# Python generates the Rust notices below.
FROM cuinterpose-builder AS cuinterpose-notices
RUN apt-get update && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
COPY compliance/rust_licenses.py compliance/test_rust_licenses.py /tmp/rust-notices/
RUN python3 -m unittest discover -s /tmp/rust-notices \
    && python3 /tmp/rust-notices/rust_licenses.py /rust-sources /rust-sources/NOTICES.txt

# Both the agent and the CPU installer test use this exact workload payload.
FROM scratch AS cuinterpose-payload
COPY --from=cuinterpose-builder /cuinterpose/build/libcuinterpose.so /usr/local/lib/snapshot/libcuinterpose.so
COPY --from=cuinterpose-builder /cuinterpose/build/libcuinterpose_core.so /usr/local/lib/snapshot/libcuinterpose_core.so
COPY --from=cuinterpose-builder /cuinterpose/build/cuinterpose-launch /usr/local/bin/cuinterpose-launch

FROM cuinterpose-notices AS cuinterpose-test
COPY --from=cuinterpose-payload / /
RUN make test-native
RUN /usr/local/bin/cuinterpose-launch --library /usr/local/lib/snapshot/libcuinterpose.so -- /bin/true

FROM scratch AS cuinterpose-export
COPY --from=cuinterpose-builder /cuinterpose/build/ /

# The GPU test binary runs on a GPU host from build/, beside the artifacts it tests.
FROM cuinterpose-builder AS cuinterpose-gpu-tests-builder
RUN cd rust \
    && cargo test --locked -p cuinterpose-gpu-tests --test gpu --target "$(uname -m)-unknown-linux-gnu" \
       --no-run --message-format=json > /tmp/gpu-tests.json \
    && executable=$(sed -n 's/.*"executable":"\([^"]*\)".*/\1/p' /tmp/gpu-tests.json) \
    && install -m 0755 "$executable" /cuinterpose-gpu-tests

# The export holds the binary with the artifacts from the same build, so they match.
FROM cuinterpose-export AS cuinterpose-gpu-tests
COPY --from=cuinterpose-gpu-tests-builder /cuinterpose-gpu-tests /cuinterpose-gpu-tests

# =============================================================================
# Stage: CRIU Builder - Build CRIU with CUDA plugin
# =============================================================================
FROM ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254 AS criu-builder

ARG CRIU_REPO
ARG CRIU_REF
ARG BUILD_JOBS=4

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
    && make -j${BUILD_JOBS} \
    && make DESTDIR=/criu-install install-criu install-lib install-cuda_plugin

COPY plugins/inet-remap/snapshot_inet_remap.c /tmp/snapshot_inet_remap.c
RUN mkdir -p /criu-install/usr/local/lib/snapshot/criu-plugins \
    && gcc -O2 -Wall -Wextra -fPIC -shared \
      -idirafter /tmp/criu/criu/include \
      -o /criu-install/usr/local/lib/snapshot/criu-plugins/snapshot_inet_remap.so \
      /tmp/snapshot_inet_remap.c

# CRIU source at the exact ref built from: this is a from-source build, and
# CRIU_REF may be a moving branch.
RUN tar -czf /criu-src.tar.gz -C /tmp --exclude-vcs criu

# =============================================================================
# Stage: Runtime deps - CRIU runtime packages
#
# Shared by the agent and sources stages so both see the same package set.
# =============================================================================
FROM ${AGENT_BASE_IMAGE} AS runtime-deps

ARG TARGETARCH

RUN case "${TARGETARCH}" in amd64|arm64) ;; *) \
      echo "ERROR: Snapshot supports amd64 and arm64, not ${TARGETARCH}" >&2; exit 1 ;; \
    esac

# Install CRIU runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    libbsd0 \
    libcap2 \
    libnet1 \
    libnl-3-200 \
    libnl-route-3-200 \
    libprotobuf32t64 \
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
# =============================================================================
# Stage: Base packages - the committed baseline still describes the base image
# =============================================================================
# The source delta below is computed against the committed baseline, so a
# base-image bump without a re-capture would silently skew it: packages the new
# base added would look like ours, and packages it dropped would vanish from
# the delta. The build would still succeed, with wrong compliance content.
#
# Checked here rather than by a `docker run` before the build. buildx already
# has this image; a Docker daemon running the check separately does not, and
# that second pull cost a minute and a half of every CI run. As a stage it is
# cached on the base digest and the baseline, so it costs nothing until one of
# them changes.
FROM ${AGENT_BASE_IMAGE} AS base-packages

ARG TARGETARCH
COPY compliance/base-packages-manifest.sh /tmp/base-packages-manifest.sh
COPY compliance/base-packages.tsv compliance/base-packages.arm64.tsv /tmp/

RUN set -e; \
    if [ "${TARGETARCH}" = arm64 ]; then baseline=/tmp/base-packages.arm64.tsv; else baseline=/tmp/base-packages.tsv; fi; \
    sh /tmp/base-packages-manifest.sh >/tmp/actual.tsv; \
    diff -u "${baseline}" /tmp/actual.tsv || { \
      echo "ERROR: ${baseline} is stale for this base image — run 'make capture-base-packages AGENT_PLATFORM=linux/${TARGETARCH}' and commit" >&2; \
      exit 1; \
    }; \
    cp "${baseline}" /tmp/baseline.tsv

FROM runtime-deps AS sources

# From the stage that checked it rather than from the context, so a stale
# baseline fails the build instead of quietly skewing the delta below.
COPY --from=base-packages /tmp/baseline.tsv /tmp/baseline.tsv
COPY compliance/collect-sources.sh /tmp/collect-sources.sh

# Debian/Ubuntu source for the delta against the NGC base image.
RUN sh /tmp/collect-sources.sh /tmp/baseline.tsv /sources/dpkg

# CRIU source, at the exact ref this image was built from.
COPY --from=criu-builder /criu-src.tar.gz /sources/criu/criu-src.tar.gz

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

# Native checkpoint/restore uses the in-repository helper.
COPY --from=cuda-helper-builder /cuda-checkpoint-helper /usr/local/bin/cuda-checkpoint-helper
COPY --from=cuinterpose-builder /cuinterpose/build/cuinterpose-coordinator /usr/local/bin/cuinterpose-coordinator
# The operator copies the launcher into source workloads. It execs the application,
# so restore needs the libraries and coordinator, but no launcher in the bundle.
COPY --from=cuinterpose-payload / /
COPY --from=cuinterpose-notices /rust-sources/ /legal/cuinterpose/rust/
COPY cmd/cuinterpose/ /legal/cuinterpose/source/
# nsmount resolves this helper at /usr/local/sbin/ns-bind-mount (see
# agent/internal/nsmount/mount.go defaultBinaryPath).
COPY --from=cuda-helper-builder /ns-bind-mount /usr/local/sbin/ns-bind-mount
RUN chmod +x /usr/local/bin/cuda-checkpoint-helper \
    /usr/local/sbin/ns-bind-mount

# Copy the built binaries
COPY --from=builder /snapshot-agent /usr/local/bin/snapshot-agent

# Assemble the injection bundle: bind-mounted read-only into the placeholder's
# mount namespace at /tmp/snapshot-binaries during restore (internal/nsmount).
# The placeholder ships none of this. nsrestore redirects all three lookup
# mechanisms into the mount: criu's libdir -> criu-plugins/ (criu.overrideLibDir),
# LD_LIBRARY_PATH -> lib/, and PATH -> the bundle root.
RUN mkdir -p /snapshot-binaries/lib /snapshot-binaries/criu-plugins
RUN mkdir -p /snapshot-binaries/snapshot-cuda \
    && install -m 0644 /usr/local/lib/snapshot/libcuinterpose.so /snapshot-binaries/snapshot-cuda/libcuinterpose.so \
    && install -m 0644 /usr/local/lib/snapshot/libcuinterpose_core.so /snapshot-binaries/snapshot-cuda/libcuinterpose_core.so \
    && install -m 0755 /usr/local/bin/cuinterpose-coordinator /snapshot-binaries/cuinterpose-coordinator
COPY --from=builder /nsrestore /snapshot-binaries/nsrestore
COPY --from=builder /snapshot-restore-proxy /snapshot-binaries/snapshot-restore-proxy
COPY --from=criu-builder /criu-install/usr/local/sbin/criu /snapshot-binaries/criu
COPY --from=criu-builder /criu-install/usr/local/lib/snapshot/criu-plugins/ /snapshot-binaries/criu-plugins/
COPY --from=cuda-helper-builder /cuda-checkpoint-helper /snapshot-binaries/cuda-checkpoint-helper

# Network binaries needed during restore:
#   ip  — address/route restore (criu/net.c)
#   tar — used by runtime.ApplyRootfsDiff
# Network lock is set to SKIP (--network-lock skip) in BuildRestoreOpts, so
# criu never forks iptables-restore/ip6tables-restore on this path and neither
# binary needs to be in the bundle. See BuildRestoreOpts for why SKIP is safe
# for restore specifically.
# ip resolves through PATH inside the namespace; tar is invoked by absolute
# path under the bundled loader (see the libc/ stage below).
RUN set -eu; \
    cp -L "$(command -v ip)" "$(command -v tar)" /snapshot-binaries/; \
    chmod +x /snapshot-binaries/criu /snapshot-binaries/nsrestore /snapshot-binaries/snapshot-restore-proxy \
             /snapshot-binaries/cuda-checkpoint-helper \
             /snapshot-binaries/ip /snapshot-binaries/tar

# Shared-library closure of the bundled binaries and plugins.
#
# glibc (libc, libm, libpthread, libdl, librt, libresolv) and the loader are
# deliberately excluded from lib/, which is on LD_LIBRARY_PATH for the directly
# exec'd binaries; tar gets its own matched pair in libc/ below. The bundle
# layout comment in agent/internal/nsmount/injector.go has the reasoning.
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
             /snapshot-binaries/tar /snapshot-binaries/criu-plugins/*.so; do \
      [ -f "$f" ] || continue; \
      if LD_LIBRARY_PATH=/snapshot-binaries/lib ldd "$f" 2>&1 | grep -q 'not found'; then \
        echo "ERROR: unresolved shared libraries in $f:" >&2; \
        LD_LIBRARY_PATH=/snapshot-binaries/lib ldd "$f" >&2; \
        exit 1; \
      fi; \
    done

# Matched loader + glibc for the bundled tar, which runtime.restoreTarCmd
# invokes as `<loader> --library-path ... tar`, where the loader is
# ld-linux-x86-64.so.2 or ld-linux-aarch64.so.1 (nsmount.BundleLoader). This is what lets
# tar run on placeholders older than AGENT_BASE_IMAGE. Keep it out of lib/ —
# see the bundle layout comment in agent/internal/nsmount/injector.go.
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) LOADER_NAME=ld-linux-x86-64.so.2 ;; \
      arm64) LOADER_NAME=ld-linux-aarch64.so.1 ;; \
    esac; \
    mkdir -p /snapshot-binaries/libc; \
    cp -L "$(ldd /snapshot-binaries/tar | awk '/ld-linux/ && !/=>/ {print $1}')" "/snapshot-binaries/libc/${LOADER_NAME}"; \
    ldd /snapshot-binaries/tar | awk '/=> \//{print $3}' \
    | grep -E '/(libc|libm|libmvec|libdl|libpthread|librt|libresolv)[.-]' \
    | xargs -r -I{} cp -Ln {} /snapshot-binaries/libc/

# Post-condition: every library the loader-invoked tar resolves must come from
# the bundle. Asserting "not found" is not enough — this stage's own glibc
# satisfies the loader just as well, so an omitted libc/ would build green here
# and only fail on a placeholder older than AGENT_BASE_IMAGE, which is the bug
# this arrangement exists to prevent.
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) LOADER=/snapshot-binaries/libc/ld-linux-x86-64.so.2 ;; \
      arm64) LOADER=/snapshot-binaries/libc/ld-linux-aarch64.so.1 ;; \
    esac; \
    LIBPATH=/snapshot-binaries/libc:/snapshot-binaries/lib; \
    escaped=$("$LOADER" --library-path "$LIBPATH" --list /snapshot-binaries/tar \
              | awk '/=>/{print $3}' | grep -v '^/snapshot-binaries/' || true); \
    if [ -n "$escaped" ]; then \
      echo "ERROR: bundled tar resolves outside the bundle:" >&2; \
      echo "$escaped" >&2; \
      "$LOADER" --library-path "$LIBPATH" --list /snapshot-binaries/tar >&2; \
      exit 1; \
    fi; \
    "$LOADER" --library-path "$LIBPATH" /snapshot-binaries/tar --version >/dev/null

COPY --from=sources /sources /legal/source

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
# Stage: Bundle export - the restore bundle on its own
# =============================================================================
# hack/verify-bundle-glibc.sh checks the bundle against an older userspace than
# the agent is built on. It used to read the bundle out of the agent image,
# which required that image to be in a Docker daemon; CI no longer loads it
# there, because unpacking a CUDA-based image costs more than the check does.
# Exporting the bundle alone keeps the check cheap:
#
#   docker buildx build --target bundle-export --output type=local,dest=<dir> .
FROM scratch AS bundle-export
COPY --from=agent /snapshot-binaries/ /
