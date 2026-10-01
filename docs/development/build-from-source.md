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
  MODEL_STREAMER_WHEEL_DIR=/path/to/pinned-wheel-directory \
  MODEL_STREAMER_S3_WHEEL_DIR=/path/to/pinned-s3-wheel-directory \
  REGISTRY=<registry> \
  VERSION=<tag>
```

This produces `<registry>/agent:<tag>`, `<registry>/pagebroker:<tag>`, and
`<registry>/operator:<tag>`. The agent and PageBroker images must be built from
the same checkout: they speak an internal protocol and the chart pulls both at
`image.agent.tag`.

The PageBroker build requires exactly one compatible Model Streamer core wheel in
`MODEL_STREAMER_WHEEL_DIR` (default: the sibling `runai-model-streamer` checkout's
`py/runai_model_streamer/dist` directory). The build checks its SHA-256 against
`agent/pagebroker/model-streamer-wheel.sha256` before extracting the native
library. It also requires exactly one S3 wheel in `MODEL_STREAMER_S3_WHEEL_DIR`
(default: `../runai-model-streamer/py/runai_model_streamer_s3/dist`), checked
against `agent/pagebroker/model-streamer-s3-wheel.sha256`. Both pins refer to
artifacts built from Model Streamer commit
`bc21fd4182cc06ce9475452d16697d50ce3588c4`, with the multi-submission native ABI.
The S3 wheel must match the core wheel's backend ABI; a wheel from a different
release is not interchangeable. The image includes `libstreamer.so`,
`libstreamers3.so`, their license metadata, and OpenSSL libcrypto for SHA-256.
The agent image build does not require these wheels.

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

These tests exercise direct AWS SDK uploads and native Model Streamer S3 reads.
See the [S3 storage contract](../../agent/pagebroker/S3.md) for configuration,
ownership, integrity and failure behavior.

Local native builds require a C++20 compiler, protobuf, GoogleTest, OpenSSL,
the pinned AWS SDK for C++ S3 component, and both pinned Streamer libraries.
The Dockerfile builds SDK 1.11.584 at commit
`bba3cfc14d4fc148aeee7a8ff7822dd7a9a0f4d3`, including its pinned submodules.
For a local installation, build that revision with `BUILD_ONLY=s3`, shared
libraries, and `CMAKE_INSTALL_LIBDIR=lib`. Run
`make -C agent/pagebroker test daemon MODEL_STREAMER_LIB_DIR=/path/to/libraries AWS_SDK_PREFIX=/path/to/aws-sdk`
for filesystem, digest, and session-recovery tests. These tests need no S3
endpoint or credentials.

To exercise S3 in the distroless runtime, build the dedicated image target with
the pinned wheel inputs described above:

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

The tests cover hashes, permissions, empty files/directories, concurrency,
multipart uploads, corruption, truncation, missing objects and endpoint errors.
Injected failures exercise retries, cleanup and uncertain remote outcomes.
`--resources` measures peak RSS and throughput in isolated uploads with a fixed
buffer budget. `--tls` uses a temporary CA and checks that both clients reject
the endpoint when that CA is missing. Moto does not enforce IAM policies.

For a locally built executable, use `make -C agent/pagebroker s3-test` with the
Python dependencies installed. The runner also accepts `--binary`; both paths
use the same disposable local service.

### PageBroker S3 checkpoint tests

Build the `s3-test` image and install the Python dependencies as above. The image
also contains the production PageBroker daemon. Build the Go test client and
run the local checkpoint fixture:

```bash
go test -c ./agent/internal/pagebroker -o /tmp/pagebroker-checkpoint-client.test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/checkpoint_integration.py \
  --image local/pagebroker:s3-test --client /tmp/pagebroker-checkpoint-client.test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/checkpoint_integration.py --tls \
  --image local/pagebroker:s3-test --client /tmp/pagebroker-checkpoint-client.test
```

The fixture runs native daemons against disposable Moto storage using fake
credentials. It exercises the real Go client, checkpoint publication, verified
and metadata-only restore, lost responses, retries without payload rewrites,
active Abort, staging ownership, expiry, restart and startup credential
resolution. It validates protocol behavior and credential selection; deployment
integration and production IAM policies require separate validation.
