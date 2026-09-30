# CUDA checkpoint helper

The agent starts the persistent helper at startup when
`cudaCheckpoint.enabled` is set. The helper owns CUDA operation handles,
primary contexts, host buffers and NIXL transfers. PageBroker remains a separate
filesystem service; it does not launch or call this helper.

`checkpoint.*` wraps the driver lifecycle; `transfer.*` owns the persistent
CUDA-copy/NIXL pipeline; `daemon.cpp` owns contexts and operation lifetimes.
`helper.cpp` selects daemon mode or the existing CLI; `client.*` implements CLI
forwarding and batch ordering. Storage and checksum code remain alongside them.

`daemon.hpp` exposes one process entry point: `RunDaemon(control_fd, options)`.
It initializes resources, serves the private phase protocol, and exits when its
owner is lost or CUDA cleanup is uncertain. A future PageBroker supervisor can
use this same runtime in its GPU worker process, including namespace admission,
session state, contexts, rings and metadata. Its parent supplies the artifact
directory and target identity through the existing descriptor protocol. This
boundary deliberately retains GPU process isolation and does not expose CUDA
execution inside PageBroker's CPU daemon.

Batch ordering lives in `client.cpp::RunBatch`: serial preparation, overlapping
restore transfers, a transfer-success barrier, then completion. A future
PageBroker API can keep these phase calls or move that small coordinator behind
a batch command. The CUDA/transfer runtime and its private C++ messages can
remain intact; no public PageBroker GPU API is introduced here.

The existing command-line operations remain available. Go invokes the helper
CLI with arguments and inherited file descriptors; `client.cpp` sends private
protobuf messages to the daemon. There are no Go protobuf bindings or new
PageBroker GPU protocol messages. The daemon calls `cuInit` and retains a
primary CUDA context for every visible device before reporting ready.
On CustomStorage-capable drivers, it also allocates and registers every
transfer ring before readiness, including when the capture policy is `driver`.
Older drivers can still run driver-managed operations without transfer rings;
they cannot restore CustomStorage artifacts.

`storageMode` selects how new checkpoints are captured. A restore follows the
saved artifact. Driver-managed operations let CUDA move its own bytes.
CustomStorage uses the persistent CUDA-copy/NIXL POSIX pipeline, with one ring
per visible GPU. Within a batch, preparation remains serial while restores
overlap a previous participant's transfer with the next participant's
preparation. Every transfer in the batch must succeed before any participant
is resumed. Sessions sharing a GPU serialize access to its ring; node-wide
storage admission remains future PageBroker work.

`enableChecksumDigest` defaults to false. When enabled, the helper hashes GPU
bytes during their existing transfer and verifies saved digests before resume.
Manifest identity and file-size checks always run. Cancellation stops new
submissions and drains outstanding work before artifact cleanup.

## Memory requirements

The default ring is 32 buffers of 128 MiB per visible GPU: 4 GiB per GPU, or
32 GiB on an eight-GPU node. Increase `daemonset.resources` before enabling the
helper; the default agent memory limit is 4 GiB. Budget additional space for
the agent, CRIU and driver-managed checkpoint memory. `maxPinnedBytes` limits
the total ring allocation, including CUDA allocation rounding; zero leaves
the budget uncapped. Exceeding the cap fails startup rather than reducing the
ring silently.

## Build and test

Production builds need CUDA 13.4 headers, a C++20 compiler, protobuf, OpenSSL
and the NIXL POSIX backend. Runtime CUDA comes from the host driver. The agent
Dockerfile builds and packages these dependencies without changing the agent
base image.

```sh
make -C agent/cmd/cuda-checkpoint-helper helper test \
  CUDA_ROOT=/usr/local/cuda NIXL_ROOT=/usr/local
```

Tests use mocked CUDA; the transfer tests use real NIXL when `NIXL_ROOT` is
provided. The production build requires NIXL. `test-storage` needs only a C++20
compiler and OpenSSL development files. The agent runs these tests when local
prerequisites are available and requires them in CI.
