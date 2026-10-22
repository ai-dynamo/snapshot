# Building from source

This guide builds the Snapshot operator and node-agent images from a checkout of
this repository and installs the chart against them. Most users should install
[from a release](../../README.md#from-a-release) instead — build from source when developing Snapshot or testing unreleased changes.

## Prerequisites

In addition to the [runtime prerequisites](../../README.md#prerequisites), the build needs:

- Go (matching the version pinned in the modules)
- Docker with Buildx
- A container registry the cluster can pull from, and push access to it
- `kubectl` and `helm` configured against the cluster

The node agent is **x86_64 (amd64) only** — `cuda-checkpoint` ships no other
architecture — so its image builds for `linux/amd64`.

## 1. Clone the repository

```bash
git clone https://github.com/ai-dynamo/snapshot.git
cd snapshot
```

## 2. Build the images

The root `Makefile` builds all three images. Override `REGISTRY` and `VERSION`
to tag them for the registry:

```bash
make docker-build-agent docker-build-pagebroker docker-build-operator \
  REGISTRY=<registry> \
  VERSION=<tag>
```

This produces `<registry>/agent:<tag>`, `<registry>/pagebroker:<tag>`, and
`<registry>/operator:<tag>`. The agent and PageBroker images must be built from
the same checkout: they speak an internal protocol and the chart pulls both at
`image.agent.tag`.

## 3. Push the images

Push the images to a registry the cluster can pull from:

```bash
docker push <registry>/agent:<tag>
docker push <registry>/pagebroker:<tag>
docker push <registry>/operator:<tag>
```

## 4. Install the chart against the built images

Install the chart from the checkout, pointing the images at the built ones:

```bash
helm install snapshot ./charts/snapshot \
  --namespace snapshot --create-namespace \
  --set image.operator.repository=<registry>/operator \
  --set image.operator.tag=<tag> \
  --set image.agent.repository=<registry>/agent \
  --set image.agent.tag=<tag> \
  --set image.pageBroker.repository=<registry>/pagebroker
```

See [Installation](../operations/install.md) for storage and uninstall options.

## Development workflow

Common `make` targets from the repo root:

- `make build` — compile the agent and operator
- `make test` — run unit tests across the `api`, `agent`, and `operator` modules
- `make lint` — run linters
- `make helm-lint` — lint the Helm chart
- `make check` — the full pre-merge gate (generate, license headers, fmt, tidy, lint, and more)

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for the contribution process and DCO
sign-off.

### PageBroker native S3 upload and restore tests

These tests exercise CRT-managed AWS SDK uploads and native Model Streamer S3 reads.

Local native builds require a C++20 compiler, protobuf, GoogleTest, OpenSSL,
the pinned AWS SDK for C++ S3 CRT component, and both pinned Streamer libraries.
The Dockerfile builds SDK 1.11.584 at commit
`bba3cfc14d4fc148aeee7a8ff7822dd7a9a0f4d3`, including its pinned submodules.
Before configuring a local SDK build, apply the required cancellation patch
from the Snapshot repository root:

```bash
git -C /path/to/aws-sdk-cpp apply \
  "$PWD/agent/pagebroker/patches/aws-sdk-crt-cancellation.patch"
```

The patch keeps queued cancellations from accessing a completed request after
it is released. Rebuild and reinstall any SDK built without it. The Dockerfile
applies the same patch automatically.

Build the patched revision with `BUILD_ONLY=s3-crt`, `USE_CRT_HTTP_CLIENT=ON`,
shared libraries, and `CMAKE_INSTALL_LIBDIR=lib`. Use headers and libraries
from the same Model Streamer installation:

```bash
make -C agent/pagebroker test daemon \
  MODEL_STREAMER_INCLUDE_DIR=/path/to/model-streamer/include \
  MODEL_STREAMER_LIB_DIR=/path/to/model-streamer/lib \
  AWS_SDK_PREFIX=/path/to/aws-sdk
```

This runs filesystem, session-recovery, and S3 client tests. These tests need
no S3 endpoint or credentials.

To exercise S3 in the distroless runtime, build the dedicated image target;
the Dockerfile downloads the pinned SDK:

```bash
make docker-build-pagebroker REGISTRY=local VERSION=s3-test \
  DOCKER_BUILD_ARGS="--load --target s3-test"
python3 -m venv /tmp/pagebroker-s3-tests
/tmp/pagebroker-s3-tests/bin/pip install -r agent/pagebroker/tests/requirements.txt
PATH="/tmp/pagebroker-s3-tests/bin:$PATH" make -C agent/pagebroker s3-fixture-test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/s3_integration.py \
  --resources --image local/pagebroker:s3-test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/s3_integration.py \
  --tls --image local/pagebroker:s3-test
```

The runner starts disposable local Moto storage with fixed test credentials,
ignoring inherited AWS credentials, profiles, endpoints and configuration files.
The Linux Docker runner uses host networking to reach it. Python prepares the
isolated reader fixtures; C++ backend uploads populate separate round-trip
prefixes, which the native Model Streamer restores.

The tests independently compare payload bytes and cover permissions, empty
files/directories, concurrency, multipart uploads, truncation, missing objects,
and endpoint errors. PageBroker does not perform application-level payload
integrity verification.
Injected failures exercise retries, cleanup and uncertain remote outcomes.
`--resources` measures peak RSS and throughput in isolated uploads with a fixed
buffer budget. `--tls` uses a temporary CA and checks that both clients reject
the endpoint when that CA is missing. Moto does not enforce IAM policies.

For a locally built executable, use `make -C agent/pagebroker s3-test` with the
Python dependencies installed. The runner also accepts `--binary`; both paths
use the same disposable local service.

The PageBroker image build also runs `make -C agent/pagebroker s3-lifetime-test`.
This checks the pinned native SDK's CPU/S3 buffer lifetime with a controlled local
HTTP server: one submission finishes while another remains blocked, and the
completed buffers become inaccessible before the other submission continues.
The cases cover success, retry, and a later submission's failure. Each case runs
in a separate process with a timeout, so a late native memory access or blocked
teardown fails the test. It requires Python 3 and the installed SDK, without
additional Python packages, cloud credentials, or a GPU. For an SDK installed
elsewhere, pass `MODEL_STREAMER_LIB_DIR=/path/to/sdk/lib`.
