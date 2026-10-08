<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Persist inference compiler caches

The vLLM, SGLang, and TensorRT-LLM capture and restore recipes mount the
[`snapshot-compiler-cache` PVC](compiler-cache-pvc.yaml). Create it before either
capture path. Use ReadWriteMany storage, such as VAST on nscale, and select
`storageClassName` if your default storage class does not support RWX.
The example requests 100 GiB. Size it for the models and engine versions you run.

Each engine uses a separate `compiler-cache/<engine>` directory on the PVC,
mounted at `/compile-cache`. Capture environment variables redirect PyTorch
Inductor, Triton, CUDA, FlashInfer, and the applicable engine caches there.
The restored process retains those environment variables, so restore manifests
only need the matching PVC mounts.

Two fixed paths also need PVC mounts:

| Path | Recipes | Reason |
| --- | --- | --- |
| `/root/.cache/flashinfer` | All engines | Covers FlashInfer artifacts that use the image's fixed default path. |
| `/tmp/torchinductor_root` | TensorRT-LLM | The pinned PyTorch build's precompiled headers use this directory even when `TORCHINDUCTOR_CACHE_DIR` is set. |

These paths use subdirectories of the same compiler PVC. Keep the same claim,
subpaths, and container mount paths for capture and every restore. To reuse an
existing RWX PVC, change the `compiler-cache` volume's `claimName` in both the
capture and restore manifests. It may share the model-cache PVC.

Compiler files on the PVC are external checkpoint dependencies. Retain them
until every checkpoint referencing them is retired. Do not clear or replace
cached libraries while those checkpoints remain in use. Use a separate cache
claim for independent engine upgrades that may rewrite those files.

Caching avoids rebuilding artifacts that the engine can reuse. A new capture
still loads the model and performs its warmup. Restore resumes the saved process
with its compiled code, CUDA graphs, and external cache paths intact.
