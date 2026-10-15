<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# cuinterpose GPU tests

These tests run the packaged frontend, core, and coordinator against the installed CUDA
driver on two GPUs. The CPU tests live with each crate and run in `make test`: protocol
framing in `protocol/tests`, topology rules in `coordinator/src/topology.rs`, and
coordinator orchestration against scripted participants in `coordinator/tests`.

## Running

| Needs | Notes |
| --- | --- |
| Artifacts from `make build` | Or set `CUINTERPOSE_BUILD_DIR` |
| Two peer-capable GPUs and a CUDA 13.1+ driver | Missing hardware fails the run instead of skipping it |
| `libcudart.so.13` and a C compiler | For the runtime and loader fixtures |

From `agent/cmd/cuinterpose`:

```sh
make test-gpu
```

On hardware without CUDA multicast or POSIX-shareable HOST_NUMA VMM, skip those tests:

```sh
make test-gpu TEST_ARGS="--skip multicast --skip host_numa"
```

A host without Rust can run a prebuilt binary. `make gpu-tests` writes the artifacts and
`build/cuinterpose-gpu-tests`, which tests the artifacts beside it, from one build. Copy
`build/` to the GPU host and run `build/cuinterpose-gpu-tests --test-threads=1`.

## How a test runs

The frontend must be preloaded before a process resolves any CUDA symbol, so the test
process never runs a scenario itself. A test starts itself again in a fresh process with
the frontend preloaded, and the re-run test function runs the scenario. Tests with
several processes dispatch on a role name. `src/process.rs` describes the mechanism.
`shim::checkpoint` drives the coordinator and, from the test process, the native
`cuCheckpointProcess*` cycle around the scenario processes. It does not run CRIU or
the agent.

| Module | Covers |
| --- | --- |
| `symbol_resolution` | Symbol binding and lookup through the frontend |
| `core_registration` | Core registration and startup with missing driver symbols |
| `control_socket` | Control socket startup, fork, exec, and existing paths |
| `control_replies` | Out-of-order requests and lost replies |
| `allocation_tracking` | Tracked VMM handles, converted mallocs, and rejected calls |
| `access_replay` | Access updates and their replay after reconstruction |
| `context_teardown` | Context destruction and primary-context reset or release |
| `foreign_imports` | Imports from processes without the frontend |
| `creator_released_backing` | Shared backing whose creator released every reference |
| `host_numa_backing` | Creator-owned HOST_NUMA backing with CPU aliases |
| `legacy_ipc` | Legacy IPC handles opened by another process |
| `malloc_peer_access` | Peer access grants on converted mallocs |
| `multicast_restore` | Multicast bindings, including nonzero offsets, through a native checkpoint |
| `multicast_bind_ordering` | Multicast bind and unbind ordering |
| `multicast_sharing` | A multicast object shared by processes that each bind one GPU |
| `peer_carriers` | Unexported allocations that another GPU can access |
