# Building from Source

This guide builds the Snapshot operator and node-agent images from a checkout of
this repository and installs the chart against them. Most users should install
[From a Release](../../README.md#from-a-release) instead. Build from source when developing Snapshot or testing unreleased changes.

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

## 1. Clone the Repository

```bash
git clone https://github.com/ai-dynamo/snapshot.git
cd snapshot
```

## 2. Build the Images

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

## 3. Push the Images

Push the images to a registry the cluster can pull from:

```bash
docker push <registry>/agent:<tag>
docker push <registry>/pagebroker:<tag>
docker push <registry>/operator:<tag>
```

## 4. Install the Chart Against the Built Images

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

See [Installation](../get-started/installation.md) for storage and uninstall options.

## Development Workflow

Common `make` targets from the repo root:

- `make build` - Compile the agent, the operator, and the coordinator module
- `make test` - Run unit tests across every Go module
- `make lint` - Run linters
- `make helm-lint` - Lint the Helm chart
- `make check` - The full pre-merge gate (generate, license headers, fmt, tidy, lint, and more)

The PageBroker image build runs its native unit tests without a local S3 service,
Python test harness, GPU, or cluster.

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for the contribution process and DCO
sign-off.
