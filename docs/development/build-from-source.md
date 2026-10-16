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

The PageBroker Dockerfile downloads the Linux x86_64 C++ SDK from
[Model Streamer 0.17.0](https://github.com/dsx-ai-factory/model-streamer/releases/tag/0.17.0)
and verifies its committed SHA-256 checksum before extracting the headers and
core library. Image builds need no separate SDK preparation, Model Streamer
compilation, or Python wheel installation. The image includes the matching
license, release metadata, and corresponding source archives. The agent image
build does not depend on the SDK.

For native builds, export the same SDK from the PageBroker Dockerfile:

```bash
docker buildx build --platform linux/amd64 \
  -f agent/pagebroker/Dockerfile --target model-streamer-sdk \
  --output type=local,dest=/tmp/snapshot-model-streamer-sdk agent/pagebroker/

make -C agent/pagebroker test daemon \
  MODEL_STREAMER_INCLUDE_DIR=/tmp/snapshot-model-streamer-sdk/include \
  MODEL_STREAMER_LIB_DIR=/tmp/snapshot-model-streamer-sdk/lib
```

PageBroker submits CPU destinations and needs no CUDA driver. The filesystem
strategy defaults to the library's ordered
`io_uring_direct,libaio_direct,sync_buffered` preference list. Use `RUNAI_STREAMER_FS_STRATEGY` before starting PageBroker to select an explicit
list such as `io_uring_buffered,sync_buffered`.

Native sessions admit at most eight submissions and target 10 GB of submitted
bytes; one larger file runs alone. Admission closes on the first completed
submission. All destinations remain mapped until the session ends, then pending
work starts a new session. Terminal failures stop native access before releasing
any affected restore; ordinary completed storage errors fail their own restore.

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
