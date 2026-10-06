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
For a digest pin, set an image's `tag` field to `<tag>@sha256:<digest>`. The
agent field supplies the same pinned image to the DaemonSet and SnapshotJob's
CuInterpose installer.

Agent digest pins currently require `pageBroker.enabled=false`. The PageBroker
sidecar uses a separate image repository but inherits the agent tag, so an agent
digest cannot identify its image. When enabling PageBroker, use the matching
release tag for both images.

## Development workflow

Common `make` targets from the repo root:

- `make build` — compile the Go agent and operator binaries
- `make -C agent cuinterpose-build` — build the C/Rust cuinterpose libraries,
  static coordinator, and static launcher in a pinned Docker builder
- `make -C agent cuinterpose-test` — containerized Rust unit, protocol,
  coordinator, launcher, and ELF artifact checks (no GPU required)
- `make test` — run unit tests across the `api`, `agent`, and `operator` modules
- `make lint` — run linters
- `make helm-lint` — lint the Helm chart
- `make check` — the full pre-merge gate (generate, license headers, fmt, tidy, lint, and more)

CuInterpose exports `libcuinterpose.so`, `libcuinterpose_core.so`,
`cuinterpose-coordinator`, and `cuinterpose-launch` into
`agent/cmd/cuinterpose/build/`. Both executables use the existing musl target. See the
[Rust build instructions](../../agent/cmd/cuinterpose/rust/README.md) for local
toolchain requirements and `test-native`.

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for the contribution process and DCO
sign-off.
