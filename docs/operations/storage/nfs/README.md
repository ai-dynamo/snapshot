<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NFS

Part of [Storage best practices](../README.md). Initial outline.

## Direct I/O (`O_DIRECT`)

TODO: When to use it, filesystem support, and the Snapshot settings that apply
to CPU images and GPU payloads.

## Buffered I/O

TODO: When to use it, page-cache behavior, memory requirements, and the settings
that apply. Distinguish cold and warm restores.

## Mount options

TODO: NFS version, `rsize`/`wsize`, connection count, and how to inspect the
effective mount options. Keep vendor-specific options in the examples below.

## Examples

- [VAST](vast.md)
- [Amazon EFS](efs.md)

## Validate a configuration

TODO: Checkpoint and restore with inference verification; record storage and
client versions, mount options, I/O mode, cache state, and restore timings.
