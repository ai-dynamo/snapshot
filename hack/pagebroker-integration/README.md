<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PageBroker integration tests

This suite builds on the complete PageBroker S3 stack. Component unit tests stay
with their implementation and run through `make -C agent/pagebroker test`.
Python fixtures and tests using the real daemon, rclone, or Model Streamer live
here, separately from those unit tests.

From the repository root on Linux/amd64, with Docker Buildx, Go, Python 3 with
venv support, and OpenSSL installed:

```sh
make pagebroker-integration-test REGISTRY=local VERSION=integration
```

The target builds the production image and its matching dependency image, then:

- Runs the Python fixture tests and native SDK buffer-lifetime checks.
- Builds and runs daemon socket, descriptor-passing, and shutdown tests.
- Runs native upload and restore tests against disposable Moto HTTP/HTTPS
  endpoints, including injected failures, cancellation, and memory limits.
- Repeats the S3 checks against the production runtime and trust store.
- Builds the Go integration client and tests the packaged daemon's checkpoint
  lifecycle over HTTP and HTTPS, including publication recovery and restart.

Fixtures use fake credentials and clean up their services and containers.
No cloud account, GPU, or Kubernetes cluster is needed. The separate
`pagebroker-integration` CI job runs this command.

For a configured native build environment, run only the native suites with:

```sh
make -C agent/pagebroker -f Makefile \
  -f ../../hack/pagebroker-integration/tests.mk integration-test
```

To repeat only the Go-client checkpoint tests against an already built image:

```sh
make pagebroker-checkpoint-test REGISTRY=local VERSION=integration
```

The Go integration source remains in `agent/internal/pagebroker` for access to
its package helpers, but is guarded by the `integration` build tag. Ordinary
`make test` does not compile or run it. Hardware GPU tests already present on
main and the separate k3s/endurance/benchmark suites keep their own workflows.
