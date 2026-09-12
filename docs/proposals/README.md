<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Snapshot Enhancement Proposal (SEP)

A Snapshot Enhancement Proposal (SEP) is the design document for a significant
Snapshot improvement, new feature, or change. It explains the intended change
and its motivation, then records the design, trade-offs, and validation plan.
SEPs give maintainers and the community a focused place to discuss a change
before implementation begins.

## How to file an SEP

Submit SEPs as Markdown files in a GitHub pull request. Follow these rules:

1. Create a [tracking issue](https://github.com/ai-dynamo/snapshot/issues).
   Its issue number is also the SEP number.
2. Create a descriptively named directory under `docs/proposals`, prefixed with
   that number—for example, `docs/proposals/123-multi-gpu-support`.
3. Name the proposal document `README.md`.
4. Start with the [SEP template](NNNN-template/README.md).
5. Generate the table of contents with `make update-toc` before opening or
   updating the pull request. Check it without modifying files by running
   `make verify-toc`.

> SEP is inspired by the Kubernetes Enhancement Proposal (KEP) process and the
> Grove Enhancement Proposal (GREP) convention.

## Why does an SEP directory start with a number?

The directory prefix is the tracking issue number. It supplies a stable,
unique identifier for the SEP and connects the design directly to the issue
where its status and implementation work are tracked.
