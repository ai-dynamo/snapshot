<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

io_uring benchmark — complete

Package note: `startup/`, `checkpoints/`, `recovery/`, `hardening/live/` and
`gpu-policy-relaxation/` refer to the original experiment archives. These
receipt directories are not included in this package.

Accepted 540 of 540 planned cells on Qwen/Qwen3-0.6B.

Each cell uses 30 seconds of warmup, 90 seconds of measurement and up to 60 seconds of drain.

Primary workload: 128 input tokens, 16 output tokens, concurrency 128. Each comparison plans five launch/restore repeats. Following the approved GPU-policy amendment, new conditions may use any of the eight verified full H100 80-GB GPUs on the same node. Each measurement keeps one physical GPU throughout; GPU IDs and policy strata are recorded.

| Serving path | Comparison | Throughput change | 95% interval | Assessment |
|---|---|---:|---:|---|
| dynamo-vllm | blocked / allowed | -0.13% | -4.12% to +4.03% | inconclusive for the 3% regression margin |
| dynamo-vllm | restored-blocked / blocked | +0.35% | -2.44% to +3.22% | interval excludes a >3% regression |
| sglang | blocked / allowed | -0.53% | -5.59% to +4.79% | inconclusive for the 3% regression margin |
| sglang | restored-blocked / blocked | +0.84% | -3.79% to +5.69% | inconclusive for the 3% regression margin |
| vllm | blocked / allowed | +0.40% | -1.02% to +1.84% | interval excludes a >3% regression |
| vllm | restored-blocked / blocked | -0.13% | -2.16% to +1.93% | interval excludes a >3% regression |

The CSV includes per-run throughput, latency, TTFT, ITL and client CPU measurements. The JSON includes all workloads/concurrencies and individual paired ratios. Intervals are per comparison; five repeats provide limited precision. Rows with fewer than 1000 requests have sparse p99 estimates. Latency, TTFT and ITL are in milliseconds.

Dynamo scope: GPU worker policy varied; frontend configuration, node, image, resources and allowed policy matched. Its chart-default frontend has a one-CPU limit and showed quota throttling in the pilot. Results describe this complete endpoint configuration and do not isolate unlimited-worker peak throughput.

Native checkpoint restore adds its own seccomp layer. Startup/capture/restore evidence is saved separately under startup/ and checkpoints/; steady-state measurement follows matched warmup. The phases and engine versions are analyzed independently.

Native sleep/memory saver is enabled in every condition. Standalone policy
applies to the native HTTP server and engine processes; the ingress and Istio
proxy configuration stays fixed. Concurrency is measured through 128; capacity
beyond that range is unmeasured. The completed 8B and 27B phases are analyzed
separately; their data is not pooled here.

Allowed queues showed sampled counter advances in these server-container observations: dynamo-vllm: 0/120; sglang: 60/60; vllm: 60/60. These counters describe the exercised paths; they do not decode SQE operations or establish results for disk-heavy workloads.

Pilot rates used different GPU UUIDs and initial client settings; they are excluded from these comparisons.

Internal sources (access restricted): CCL-590 and the CentML/platform PR4684
discussion. The public synthesis is in [the benchmark report](../../REPORT.md).


Execution interruption: a deadline-only extension triggered the original runner's in-memory integrity check after 38 validated cells. Cleanup completed, and the operator explicitly approved recovery. All 38 exports passed a fresh hash check. The replacement client matches the original image, node, CPU/memory allocation and exact dependencies. Dynamo repeat 1 allowed prefill cells at concurrency 1 and 8 precede the interruption; its remaining cells use a new worker launch. The primary 128/16 concurrency-128 comparison is outside that interrupted block. Recovery preflight data is excluded from formal comparisons. Original provenance and amendment receipts are retained under recovery/.


Further recovery: the second attempt stopped after 40 accepted cells when client telemetry retrieval timed out. That interrupted AIPerf cell completed successfully, but its missing telemetry excludes it from comparisons; its raw data is archived. Continuation uses detached execution and durable recovery journals with unchanged AIPerf arguments. Live native Dynamo restore, lost launch acknowledgement, and telemetry timeout recovery passed before continuation. All validation data is excluded from formal comparisons. See hardening/live/ for the validation and amendment receipts.


Hardware-policy amendment: the original results use fixed per-path GPU UUIDs. Later results use the user-approved verified H100 pool with identical model, memory, driver, node, resources and MIG-disabled checks. Cross-condition GPU assignments may differ. Pooled intervals include hardware variation and combine these assignment policies; the JSON separately reports policy strata, same-GPU pair counts, and each GPU pair. Do not interpret a pooled difference as an isolated same-device effect. The first 144 accepted cells were preserved without modification. See gpu-policy-relaxation/ for the inventory and amendment receipt.
