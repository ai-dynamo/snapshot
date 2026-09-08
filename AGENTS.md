<!--
SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# AGENTS.md

Guidance for AI coding agents working in this repository. Human contributors
should start with [CONTRIBUTING.md](CONTRIBUTING.md); everything there applies
here too.

> Note: "agent" in this file means an AI coding assistant. The `agent/` module
> is a different thing entirely — the node-level Snapshot agent daemon.

## What this project is

Snapshot is a Kubernetes-native checkpoint and restore system for NVIDIA GPU
workloads. It checkpoints a fully initialized GPU pod — running process, CPU and
GPU memory — and restores that state on another compatible node. It provides
primitives, not orchestration.

## Repository layout

| Path | Go module | What lives there |
| --- | --- | --- |
| `api/` | yes | CRD types (`PodSnapshot`, `PodSnapshotContent`, `SnapshotJob`) and generated CRD YAML in `api/v1alpha1/crds` |
| `agent/` | yes | Node-level daemon that drives CRIU and CUDA checkpointing. **Linux/amd64 only** |
| `operator/` | yes | Cluster controller that reconciles the CRDs |
| `charts/snapshot/` | — | Helm chart; `charts/snapshot/crds` is a generated copy of the CRDs |
| `e2e/` | — | Python end-to-end suite (uv/pyproject), not Go |
| `docs/` | — | User and developer documentation |
| `hack/` | — | Build tooling, license boilerplate, base-package capture |

Three separate Go modules — `api`, `agent`, and `operator` — each with its own
`go.mod` and its own `Makefile`. The root `Makefile` fans out to all three.

## Build and test

Run everything from the repository root. Go 1.26.6.

```bash
make check        # default goal: the full local gate (see below)
make test         # unit tests across api, agent, operator
make build        # build agent and operator binaries
make lint         # golangci-lint across all three modules
make fmt          # format
make generate     # regenerate CRDs from api/ types
```

`make check` is the one that matters before opening a pull request. It runs, in
order: `verify-crds`, `install-tools`, `generate`, `pagebroker-check-generated`,
`add-license-headers`, `fmt`, `tidy`, `verify-license-headers`, `lint`,
`govulncheck`, and `helm-lint`. It deliberately does not run in parallel
(`.NOTPARALLEL:`) because the stages mutate files that later stages read.

To scope work to one module, use its own Makefile — `make -C operator test`.

### Building the agent from macOS

The agent is Linux/amd64-only (`cuda-checkpoint` ships no other architecture),
so it does not build natively on a Mac. Use the containerized targets:

```bash
make linux-build   # builds agent/ inside a golang container
make linux-test    # runs agent/ tests inside a golang container
```

Both require Docker.

## Conventions that will fail CI if you miss them

1. **Every commit must be signed off (DCO).** Use `git commit -s`. Unsigned
   commits fail the DCO check and cannot be merged. See
   [CONTRIBUTING.md](CONTRIBUTING.md#developer-certificate-of-origin-dco).

2. **Every pull request must link an approved issue.** The
   `Validate Issue Reference` check fails when a pull request references no
   issue, or only issues that are closed or not yet labeled `approved`. Open the
   issue first and wait for a maintainer to apply `approved` before investing
   significant work. Do not open a pull request that skips this step.

3. **SPDX license headers are required on new files.** `make add-license-headers`
   adds them from `hack/boilerplate.addlicense.txt`; `verify-license-headers`
   checks them in CI. Generated CRDs are exempt.

4. **CRDs are generated, not hand-edited.** Edit the types under `api/v1alpha1`,
   then run `make generate`. The chart copy in `charts/snapshot/crds` must stay
   in sync with `api/v1alpha1/crds` — `verify-crds` fails on drift and tells you
   to run `make generate` and commit the result.

5. **The agent base image is pinned in one place.** `AGENT_BASE_IMAGE` is read
   out of `agent/Dockerfile`. If you change it, re-run
   `make capture-base-packages` so the committed package baseline matches;
   `verify-base-packages` enforces this.

## Working style

- Match the surrounding code. The repository favors explanatory comments on
  non-obvious decisions — see the `.NOTPARALLEL:` and `AGENT_BASE_IMAGE`
  comments in the root `Makefile` for the register to aim for.
- Add or update tests alongside behavior changes. Controller logic under
  `agent/internal` and `operator/internal` has substantial table-driven test
  coverage; follow the existing patterns rather than inventing new harnesses.
- Do not edit generated files (`zz_generated*.go`, both CRD directories).
- Do not bump dependencies as a side effect of an unrelated change; Dependabot
  handles routine updates.
- Never put a security vulnerability in a public issue or pull request — follow
  [SECURITY.md](SECURITY.md).

## Where to look first

- [README.md](README.md) — what the system does and how it is used
- [docs/development/build-from-source.md](docs/development/build-from-source.md) — full build instructions
- [docs/reference](docs/reference) — API reference
- [docs/operations](docs/operations) — install, storage, and operational guides
- [e2e/README.md](e2e/README.md) — running the end-to-end suite
