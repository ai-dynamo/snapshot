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

Build the pinned Model Streamer inputs first:

```bash
make model-streamer-artifacts
```

This builds the x86_64 core wheel and public C headers from commit
`ae93548e02be46f479e7689a7e10e9bb243bb653`, using the digest-pinned upstream
toolchain in `agent/pagebroker/model-streamer.Dockerfile`. Fixed package version
and ZIP timestamps make the wheel reproducible. The output defaults to
`.model-streamer/`; override `MODEL_STREAMER_ARTIFACT_DIR` for another location.
CI runs this same target before building PageBroker.

`docker-build-pagebroker` verifies the wheel and public headers against the
committed SHA-256 pins. `MODEL_STREAMER_WHEEL_DIR`, `MODEL_STREAMER_INCLUDE_DIR`
and `MODEL_STREAMER_LEGAL_DIR` can select separately provisioned matching inputs.
The image includes the native library, metadata and corresponding source
materials. The agent image build does not require Model Streamer.

For native builds, pass `MODEL_STREAMER_INCLUDE_DIR=/path/to/include` and
`MODEL_STREAMER_LIB_DIR=/path/to/lib` to `make -C agent/pagebroker test daemon`.
PageBroker submits CPU destinations and needs no CUDA driver. The filesystem
strategy defaults to the library's `sync_buffered`; set
`RUNAI_STREAMER_FS_STRATEGY` before starting PageBroker to select an explicit
preference list such as `io_uring_buffered,sync_buffered`.

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
