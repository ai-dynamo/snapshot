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

The node agent supports `linux/amd64` and `linux/arm64`. The agent,
PageBroker and operator images build for `linux/amd64` by default; pass
`AGENT_PLATFORM=linux/arm64` to build them for arm64 nodes. Release images are
published for both architectures; builds from `main` are amd64 only.

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

### Native S3 read tests

Build the `s3-test` Docker target; it downloads the pinned SDK. Install
`agent/pagebroker/tests/requirements.txt` in a virtual environment, then run
`agent/pagebroker/tests/s3_integration.py --image local/pagebroker:s3-test`.
Add `--tls` to verify the temporary CA and rejection of untrusted TLS. The
fixture uses disposable local Moto storage and fake credentials. Payloads are
read by the native Model Streamer plugin. The tests independently compare restored
bytes with their source fixtures and check sizes, permissions, and error handling.
PageBroker does not perform application-level payload integrity verification.

The PageBroker image build also runs `make -C agent/pagebroker s3-lifetime-test`.
This checks the pinned native SDK's CPU/S3 buffer lifetime with a controlled local
HTTP server: one submission finishes while another remains blocked, and the
completed buffers become inaccessible before the other submission continues.
The cases cover success, retry, and a later submission's failure. Each case runs
in a separate process with a timeout, so a late native memory access or blocked
teardown fails the test. It requires Python 3 and the installed SDK, without
additional Python packages, cloud credentials, or a GPU. For an SDK installed
elsewhere, pass `MODEL_STREAMER_LIB_DIR=/path/to/sdk/lib`.
