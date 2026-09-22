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

### PageBroker native S3 read tests

The native restore component accepts explicit `s3://bucket/key` source locators
and optional expected SHA-256 values in an in-memory `RestorePlan`. Digests are
checked after native reads complete and before final permissions are applied.
Existing PageBroker RPCs remain filesystem-only; these tests do not define an
artifact index or expose S3 through filesystem protobuf fields.

Local native builds require a C++20 compiler, protobuf, GoogleTest, OpenSSL
development headers, and both pinned native libraries. Run
`make -C agent/pagebroker test daemon MODEL_STREAMER_LIB_DIR=/path/to/libraries`
for filesystem, digest, and session-recovery tests. These tests need no S3
endpoint or credentials.

To exercise S3 in the actual distroless runtime, build the dedicated image target:

```bash
make docker-build-pagebroker REGISTRY=local VERSION=s3-test \
  DOCKER_BUILD_ARGS="--load --target s3-test"
python3 -m venv /tmp/pagebroker-s3-tests
/tmp/pagebroker-s3-tests/bin/pip install -r agent/pagebroker/tests/requirements.txt
PATH="/tmp/pagebroker-s3-tests/bin:$PATH" make -C agent/pagebroker s3-fixture-test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/s3_integration.py \
  --image local/pagebroker:s3-test
/tmp/pagebroker-s3-tests/bin/python agent/pagebroker/tests/s3_integration.py \
  --tls --image local/pagebroker:s3-test
```

The runner always starts a disposable local Moto service and uses fixed test
credentials. Inherited AWS credentials, profiles, endpoints, and configuration
files are ignored. The Linux Docker runner uses host networking to reach the
disposable Moto service. It uploads a fixture with nested and empty directories,
zero-length objects, binary contents, special key characters, and a file larger
than the native S3 chunk size. Downloads go through the real Model Streamer S3 plugin.
The tests check hashes, permissions, concurrent restores, missing objects,
truncation, and endpoint failures, and report throughput and peak RSS.
The `--tls` run uses a temporary CA, checks verified HTTPS reads, and checks that
a separate native process without that CA rejects the endpoint.
Moto does not enforce authentication, so these tests do not validate IAM policies.

To test a locally built executable, use `make -C agent/pagebroker s3-test` with
the Python dependencies installed; `--binary` can also be passed directly to the
runner. This uses the same disposable local service.

The native component supports the following configuration. Keep it consistent
for the process lifetime; the local test runner supplies its own region,
endpoint, credentials, and CA.

| Input | Behavior |
| --- | --- |
| `ModelStreamerSessionOptions.region` / `.endpoint` | Explicit connection settings fixed when the native session starts |
| `.access_key_id`, `.secret_access_key`, `.session_token` | Optional credentials fixed for the session; omit all to use the native AWS provider chain |
| `RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING` | `0` for path addressing with compatible endpoints; otherwise the native default uses virtual addressing |
| `AWS_CA_BUNDLE` | Custom CA file for verified HTTPS; the file must be available in the runtime |
| `RUNAI_STREAMER_CONCURRENCY`, `RUNAI_STREAMER_S3_MAX_INFLIGHT_MIB` | Native worker count and S3 in-flight read window |
| `RUNAI_STREAMER_S3_MAX_RETRIES`, `RUNAI_STREAMER_S3_TIMEOUT`, `RUNAI_STREAMER_S3_REQUEST_TIMEOUT_MS` | Native retry, chunk-retry deadline, and low-speed request timeout settings |

The pinned native API has no per-submission cancellation or live credential
replacement. The wrapper preserves its existing session teardown and recovery
behavior. Its 10 GB submission target is not a total staging-memory limit.
Empty files are created locally without a remote read, so the fixture separately
checks that the empty S3 object exists. Source keys are passed literally to the
pinned URI parser, without percent-encoding; keys containing newline characters
are not supported by that parser.
