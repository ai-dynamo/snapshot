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

The PageBroker image build runs its native unit tests without a local S3 service,
Python test harness, GPU, or cluster.

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for the contribution process and DCO
sign-off.

### PageBroker S3 checkpoint tests

For daemon-level checkpoint protocol checks, use Docker, Go and Python 3 with
venv support to run:

```bash
make docker-build-pagebroker REGISTRY=local VERSION=s3-test DOCKER_BUILD_ARGS="--load"
make pagebroker-checkpoint-test REGISTRY=local VERSION=s3-test
```

The test target prepares the Python environment, builds the Go test client and
runs the HTTP and TLS fixtures. The `pagebroker-image` CI job runs this target
after loading the packaged daemon image. The repository-root `make test` runs
Go unit tests and does not run these fixtures.

The fixture runs native daemons against disposable Moto storage using fake
credentials. It exercises the real Go client, checkpoint publication, full
and metadata-only restore, lost responses, retries without payload rewrites,
active Abort, staging ownership, expiry, restart and startup credential
resolution. It validates protocol behavior and credential selection; deployment
integration and production IAM policies require separate validation.
