<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

io_uring benchmark — complete

Package note: `startup/`, `checkpoints/`, `device-verification.json` and
`gpu-policy.json` refer to the original experiment archives. These records are
not included in this package.

Accepted 540 of 540 planned cells on Qwen/Qwen3-8B.

Each cell uses 30 seconds of warmup, 90 seconds of measurement and up to 60 seconds of drain.

Primary workload: 128 input tokens, 16 output tokens, concurrency 128. Each comparison plans five launch/restore repeats. Following the approved GPU-policy amendment, new conditions may use any of the eight verified full H100 80-GB GPUs on the same node. Each measurement keeps one physical GPU throughout; GPU IDs and policy strata are recorded.

| Serving path | Comparison | Throughput change | 95% interval | Assessment |
|---|---|---:|---:|---|
| dynamo-vllm | blocked / allowed | -0.71% | -5.32% to +4.13% | inconclusive for the 3% regression margin |
| dynamo-vllm | restored-blocked / blocked | +0.01% | -3.82% to +3.99% | inconclusive for the 3% regression margin |
| sglang | blocked / allowed | +0.34% | -0.32% to +1.01% | interval excludes a >3% regression |
| sglang | restored-blocked / blocked | -0.24% | -1.04% to +0.57% | interval excludes a >3% regression |
| vllm | blocked / allowed | -0.20% | -1.07% to +0.66% | interval excludes a >3% regression |
| vllm | restored-blocked / blocked | +0.40% | -0.27% to +1.08% | interval excludes a >3% regression |

The CSV includes per-run throughput, latency, TTFT, ITL and client CPU measurements. The JSON includes all workloads/concurrencies and individual paired ratios. Intervals are per comparison; five repeats provide limited precision. Rows with fewer than 1000 requests have sparse p99 estimates. Latency, TTFT and ITL are in milliseconds.

Dynamo scope: GPU worker policy varied; frontend configuration, node, image, resources and allowed policy matched. Its chart-default frontend has a one-CPU limit and showed quota throttling in the pilot. Results describe this complete endpoint configuration and do not isolate unlimited-worker peak throughput.

Native checkpoint restore adds its own seccomp layer. Startup/capture/restore evidence is saved separately under startup/ and checkpoints/; steady-state measurement follows matched warmup. The phases and engine versions are analyzed independently.

Native sleep/memory saver is enabled in every condition. Standalone policy applies to the native HTTP server and engine processes; the ingress and Istio proxy configuration stays fixed. Concurrency is measured through 128; capacity beyond that range is unmeasured. The 0.6B phase is analyzed separately; its data is not pooled here.

Allowed queues showed sampled counter advances in these server-container observations: dynamo-vllm: 0/120; sglang: 60/60; vllm: 60/60. These counters describe the exercised paths; they do not decode SQE operations or establish results for disk-heavy workloads.

Pilot rates used different GPU UUIDs and initial client settings; they are excluded from these comparisons.

Internal sources (access restricted): CCL-590 and the CentML/platform PR4684
discussion. The public synthesis is in [the benchmark report](../../REPORT.md).


Hardware policy: this separate 8B experiment uses the user-approved verified H100 pool with identical model, memory, driver, node, resources and MIG-disabled checks. Cross-condition GPU assignments may differ. Intervals include hardware variation between condition launches; the JSON separately reports policy strata, same-GPU pair counts, and each GPU pair. Do not interpret a pooled difference as an isolated same-device effect. This 8B phase uses the verified H100 pool from its first accepted cell. See device-verification.json and gpu-policy.json for this phase's fresh inventory.
