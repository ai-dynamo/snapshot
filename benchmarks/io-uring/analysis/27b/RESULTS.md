<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

io_uring benchmark — complete

Package note: `startup/`, `checkpoints/`, `device-verification.json` and
`gpu-policy.json` refer to the original experiment archives. These records are
not included in this package.

Accepted 540 of 540 planned cells on Qwen/Qwen3.8-27B-FP8.

Each cell uses 30 seconds of warmup, 90 seconds of measurement and up to 60 seconds of drain.

Primary workload: 128 input tokens, 16 output tokens, concurrency 128. Each comparison plans five launch/restore repeats. Following the approved GPU-policy amendment, new conditions may use any of the eight verified full H100 80-GB GPUs on the same node. Each measurement keeps one physical GPU throughout; GPU IDs and policy strata are recorded.

| Serving path | Comparison | Throughput change | 95% interval | Assessment |
|---|---|---:|---:|---|
| dynamo-vllm | blocked / allowed | +0.41% | -0.41% to +1.23% | interval excludes a >3% regression |
| dynamo-vllm | restored-blocked / blocked | -0.16% | -0.98% to +0.67% | interval excludes a >3% regression |
| sglang | blocked / allowed | -0.66% | -1.92% to +0.61% | interval excludes a >3% regression |
| sglang | restored-blocked / blocked | +0.30% | -0.65% to +1.26% | interval excludes a >3% regression |
| vllm | blocked / allowed | -0.16% | -0.61% to +0.29% | interval excludes a >3% regression |
| vllm | restored-blocked / blocked | +0.03% | -2.49% to +2.62% | interval excludes a >3% regression |

The CSV includes per-run throughput, latency, TTFT, ITL and client CPU measurements. The JSON includes all workloads/concurrencies and individual paired ratios. Intervals are per comparison; five repeats provide limited precision. Rows with fewer than 1000 requests have sparse p99 estimates. Latency, TTFT and ITL are in milliseconds.

Dynamo scope: GPU worker policy varied; frontend configuration, node, image, resources and allowed policy matched. Its chart-default frontend has a one-CPU limit and showed quota throttling in the pilot. Results describe this complete endpoint configuration and do not isolate unlimited-worker peak throughput.

Native checkpoint restore adds its own seccomp layer. Startup/capture/restore evidence is saved separately under startup/ and checkpoints/; steady-state measurement follows matched warmup. The phases and engine versions are analyzed independently.

Native sleep/memory saver is enabled in every condition. Standalone policy applies to the native HTTP server and engine processes; the ingress and Istio proxy configuration stays fixed. Concurrency is measured through 128; capacity beyond that range is unmeasured. The 0.6B phase is analyzed separately; its data is not pooled here.

Allowed queues showed sampled counter advances in these server-container observations: dynamo-vllm: 0/120; sglang: 60/60; vllm: 60/60. These counters describe the exercised paths; they do not decode SQE operations or establish results for disk-heavy workloads.

Pilot rates used different GPU UUIDs and initial client settings; they are excluded from these comparisons.

Internal sources (access restricted): CCL-590 and the CentML/platform PR4684
discussion. The public synthesis is in [the benchmark report](../../REPORT.md).


Hardware policy: this separate 27B experiment uses the user-approved verified H100 pool with identical model, memory, driver, node, resources and MIG-disabled checks. Cross-condition GPU assignments may differ. Intervals include hardware variation between condition launches; the JSON separately reports policy strata, same-GPU pair counts, and each GPU pair. Do not interpret a pooled difference as an isolated same-device effect. This 27B phase uses the verified H100 pool from its first accepted cell. See device-verification.json and gpu-policy.json for this phase's fresh inventory.
