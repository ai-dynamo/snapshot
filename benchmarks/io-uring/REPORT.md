<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Performance impact of io_uring restrictions and checkpoint restore in LLM serving

Benchmark period: October 21–29, 2026.

## Abstract

We evaluated whether restricting io_uring syscalls affects LLM inference performance, and whether checkpoint restore introduces additional serving overhead. The benchmark covered vLLM, SGLang and Dynamo-vLLM, three model sizes, three request-length combinations and four concurrency levels on NVIDIA H100 GPUs.

Across the main sweeps, blocking io_uring and restoring with blocking enabled produced no consistent throughput regression. Primary throughput point estimates were all within approximately 1% of their baselines. Average request latency, time to first token, inter-token latency and CPU cost per completed request also showed no consistent blocking penalty.

Tail metrics were less precise. An initial Dynamo-vLLM p99 request-latency increase after restore motivated a separate ten-pair experiment. That follow-up did not establish a repeatable p99 penalty, but its confidence interval remained too wide to exclude a 3% degradation. The findings describe warmed serving under the tested configurations and do not establish zero overhead for all workloads.

## Experimental design

| Dimension | Configuration |
|---|---|
| Models | Qwen3-0.6B, Qwen3-8B, Qwen3.8-27B-FP8 |
| Serving systems | vLLM 0.27.1; SGLang 0.5.22; Dynamo-vLLM using vLLM 0.28.0 |
| Hardware | One full NVIDIA H100 80 GB per serving workload; MIG disabled |
| Load generator | AIPerf 0.13.0 |
| Input/output lengths | 128/16, 2048/128 and 128/512 tokens |
| Concurrent requests | 1, 8, 32 and 128 |
| Repetitions | Five launch/restore blocks per model and serving system |
| Timing | 30-second warmup, 90-second profiling, up to 60 seconds of drain |
| Client | Eight-CPU limit, 16 GiB RAM, up to eight AIPerf workers, one record processor |

Each main sweep compared three conditions:

1. **Allowed:** ordinary serving with io_uring permitted.
2. **Blocked:** ordinary serving with `io_uring_setup`, `io_uring_enter` and `io_uring_register` denied with `EPERM`.
3. **Restored-blocked:** checkpoint-restored serving with those syscalls blocked.

The blocked-versus-allowed comparison evaluates the syscall restriction. The restored-blocked-versus-blocked comparison evaluates the additional effect of the restore workflow while blocking is held constant. It does not isolate an io_uring-specific restore effect.

For standalone vLLM and SGLang, the restriction applied to the native HTTP server and engine processes. For Dynamo-vLLM, it applied to the GPU worker; the frontend retained its allowed policy and matched configuration and resources. Native sleep or memory-saver support was enabled in every condition. Ingress and mesh configuration remained matched.

Each model sweep completed 540 accepted measurement cells: three systems × three conditions × three request-length combinations × four concurrency levels × five repetitions. The three sweeps therefore contain **1,620 cells and 7,914,553 profiling requests**. Accepted measurements reported no request errors. Pilot runs and rejected measurements were excluded.

## Statistical analysis

The prespecified primary workload was **128 input tokens, 16 output tokens and concurrency 128**. Main-sweep effects are geometric means of five within-block ratios, with 95% Student-t confidence intervals calculated on log ratios. Launch/restore blocks, rather than individual requests, were the statistical units. Models and serving systems were analyzed separately.

A throughput loss greater than 3% was the prespecified practical regression threshold. An interval crossing that threshold is inconclusive even when the point estimate is near zero. An interval including zero does not establish equivalence.

Latency, token-spacing and CPU comparisons from the main sweeps were secondary and exploratory. Their intervals are individual comparisons without multiple-testing adjustment. Percentile effects compare per-run percentiles; they are not pooled-request percentiles.

## Throughput

The following table reports primary-workload percentage changes and 95% confidence intervals. Negative values indicate lower throughput.

| Model | Serving system | Blocked versus allowed | Restored-blocked versus blocked |
|---|---|---:|---:|
| 0.6B | vLLM | +0.40% [−1.02%, +1.84%] | −0.13% [−2.16%, +1.93%] |
| 0.6B | SGLang | −0.53% [−5.59%, +4.79%] | +0.84% [−3.79%, +5.69%] |
| 0.6B | Dynamo-vLLM | −0.13% [−4.12%, +4.03%] | +0.35% [−2.44%, +3.22%] |
| 8B | vLLM | −0.20% [−1.07%, +0.66%] | +0.40% [−0.27%, +1.08%] |
| 8B | SGLang | +0.34% [−0.32%, +1.01%] | −0.24% [−1.04%, +0.57%] |
| 8B | Dynamo-vLLM | −0.71% [−5.32%, +4.13%] | +0.01% [−3.82%, +3.99%] |
| 27B FP8 | vLLM | −0.16% [−0.61%, +0.29%] | +0.03% [−2.49%, +2.62%] |
| 27B FP8 | SGLang | −0.66% [−1.92%, +0.61%] | +0.30% [−0.65%, +1.26%] |
| 27B FP8 | Dynamo-vLLM | +0.41% [−0.41%, +1.23%] | −0.16% [−0.98%, +0.67%] |

All primary point estimates were within approximately 1% of baseline. All six primary 27B comparisons individually excluded a throughput loss greater than 3%. Some smaller-model comparisons, particularly Dynamo-vLLM, remained inconclusive against that threshold.

The broader 27B matrix showed the same general pattern. Across all 12 request-length/concurrency combinations per serving system, blocking throughput estimates ranged from −0.41% to +0.33% for vLLM, −0.66% to +0.20% for SGLang, and −1.20% to +1.72% for Dynamo-vLLM. Ten of twelve Dynamo blocking intervals, and all twelve vLLM and SGLang blocking intervals, individually excluded a loss greater than 3%.

## Request latency and time to first token

Blocking showed no consistent average-responsiveness penalty. Across the nine primary model/system combinations, mean request-latency changes ranged from **−0.34% to +0.77%**, and mean time-to-first-token (TTFT) changes ranged from **−1.07% to +1.29%**.

| Model | Serving system | Mean request latency, blocked/allowed | p99 request latency, blocked/allowed | Mean request latency, restored/blocked | p99 request latency, restored/blocked |
|---|---|---:|---:|---:|---:|
| 0.6B | vLLM | −0.34% | −1.16% | −0.01% | −0.27% |
| 0.6B | SGLang | +0.52% | −0.16% | −0.82% | −0.69% |
| 0.6B | Dynamo-vLLM | +0.22% | +1.18% | −0.34% | −5.16% |
| 8B | vLLM | +0.20% | +0.34% | −0.44% | −0.54% |
| 8B | SGLang | −0.34% | −1.89% | +0.23% | +2.06% |
| 8B | Dynamo-vLLM | +0.77% | +7.50% | −0.17% | −1.22% |
| 27B FP8 | vLLM | +0.22% | +1.66% | −0.08% | −0.77% |
| 27B FP8 | SGLang | +0.61% | +0.62% | −0.24% | −0.30% |
| 27B FP8 | Dynamo-vLLM | −0.34% | −2.63% | +0.17% | +6.10% |

These entries are point estimates; the accompanying analysis files contain the individual confidence intervals. Positive latency changes indicate slower responses.

None of the primary blocking comparisons for mean request latency, p99 request latency, mean TTFT or p99 TTFT had an individual 95% interval entirely above zero. Some intervals were wide: 8B Dynamo-vLLM's +7.50% p99 request-latency estimate had an interval of −16.48% to +38.37%. Its +11.66% p99 TTFT estimate had an interval of −25.30% to +66.90%.

The original 27B Dynamo-vLLM restore comparison was a notable secondary signal: p99 request latency increased **6.10% [2.33%, 10.02%]**. This motivated the separate follow-up below.

## Token spacing and streaming gaps

Across primary blocking comparisons, mean inter-token latency (ITL) changes ranged from **−1.92% to +2.12%**, with no individual 95% interval entirely above zero. Mean ITL therefore showed no consistent blocking penalty.

Tail ITL estimates were substantially noisier. For example, the 0.6B Dynamo-vLLM blocking comparison had a p99 ITL estimate of **+68.23%**, with an interval of **−28.58% to +296.25%**. This large point estimate did not establish a reproducible increase.

Some restore comparisons showed improvements, including 8B vLLM mean ITL at **−2.11% [−3.47%, −0.73%]**. Such secondary findings should not be generalized into a restore benefit across configurations.

AIPerf ITL summarizes per-request token spacing; it does not directly measure the worst individual streaming stall. The follow-up therefore also analyzed the largest observed inter-chunk gap per request. Missing gap observations were not treated as zero, and gap percentiles are conditional on requests with observed gaps.

## CPU efficiency

CPU efficiency was calculated as cgroup CPU time divided by request completions within each component's profiling-sample interval. Serving CPU cost includes the worker and, for Dynamo, the frontend. It excludes ingress and mesh sidecars. This is an approximate steady-state CPU cost, rather than whole-system resource accounting.

The 27B measurements were recalculated using this same method so that CPU comparisons use completed requests as the denominator rather than comparing CPU utilization alone.

| Model | Serving system | CPU time/request, blocked versus allowed | CPU time/request, restored-blocked versus blocked |
|---|---|---:|---:|
| 0.6B | vLLM | −0.45% | +0.21% |
| 0.6B | SGLang | +0.94% | −0.70% |
| 0.6B | Dynamo-vLLM | +0.45% | −3.07% |
| 8B | vLLM | +0.16% | −0.29% |
| 8B | SGLang | −1.27% | +1.14% |
| 8B | Dynamo-vLLM | +0.10% | −3.79% |
| 27B FP8 | vLLM | −1.67% | −0.17% |
| 27B FP8 | SGLang | +1.20% | −0.36% |
| 27B FP8 | Dynamo-vLLM | −1.92% | −2.24% |

Primary blocking estimates ranged from **−1.92% to +1.20%**. No individual interval established increased serving CPU cost from blocking.

Dynamo's restore comparisons showed reduced CPU cost for 0.6B, **−3.07% [−5.39%, −0.70%]**, and 8B, **−3.79% [−5.42%, −2.14%]**. The 27B estimate was also negative, **−2.24%**, but its interval crossed zero. These exploratory observations suggest no consistent added CPU cost; they do not establish a general efficiency benefit from restore.

## Targeted Dynamo restore experiment

The follow-up used Qwen3.8-27B-FP8 and compared ordinary blocked serving with restored-blocked serving over **ten paired repetitions**. Both conditions blocked io_uring.

Each condition passed through concurrency 1, 8 and 32 before a five-minute concurrency-128 profile. Conditioning cells used 30 seconds of warmup and 90 seconds of profiling. The concurrency-128 cell used 30 seconds of warmup and 300 seconds of profiling.

The prespecified primary endpoint was p99 request latency for requests starting **120–300 seconds into profiling**. Early and full-profile windows were secondary. These windows are relative to profiling, not time since restore; the experiment assesses serving after the preceding conditioning sequence.

The experiment completed **80 accepted cells**, including **518,327 concurrency-128 profiling requests**. The primary steady window included 310,037 requests. Effects used paired log ratios with 95% Student-t intervals across ten repetitions.

| Metric | Restored versus ordinary | 95% confidence interval |
|---|---:|---:|
| Full-profile throughput | +0.50% | −0.16% to +1.17% |
| Steady-window p99 request latency — primary | +2.95% | −4.14% to +10.56% |
| Full-profile p99 request latency | +0.52% | −4.05% to +5.30% |
| Steady-window p95 request latency | −2.07% | −6.66% to +2.75% |
| Steady-window p99 TTFT | +0.22% | −0.68% to +1.12% |
| Steady-window p99 decode duration | +5.21% | −1.54% to +12.43% |
| Steady-window p99 largest observed chunk gap | +3.20% | −15.74% to +26.39% |

The effect varied between pairs. Two pairs showed approximately 26% and 20% higher steady-window p99 after restore, while the final five pairs averaged 2.45% lower p99.

| Exploratory subset | Steady-window p99 change | 95% confidence interval |
|---|---:|---:|
| Excluding two pairs with recovery-related idle periods | +1.19% | −5.04% to +7.84% |
| Four pairs using the same physical GPU | −1.64% | −9.17% to +6.51% |
| Final five pairs | −2.45% | −6.74% to +2.04% |

The original tail-latency signal did not reproduce consistently. However, the primary interval includes degradation above the prespecified 3% latency margin, so the follow-up does not establish tail-latency non-inferiority at that threshold. Decode-duration and chunk-gap results also remain uncertain.

## Observed io_uring activity

Allowed vLLM and SGLang server observations showed sampled queue-counter advances. Blocked workers had no retained rings in accepted checks. These observations demonstrate a policy difference on exercised paths.

Dynamo's sampled allowed worker and frontend observations showed no queue-counter advances. Its results therefore provide weaker evidence about applications that actively depend on io_uring.

Queue counters do not identify submitted operation types, quantify syscall execution time or establish a particular fallback mechanism. The benchmark supports endpoint performance conclusions rather than an explanation of why performance remained similar.

## Validation and limitations

Accepted main-sweep measurements passed their acceptance validators and reported
no request errors. For the targeted follow-up, the archived validation run
verified all 80 raw-export hashes and recomputed the report from raw records.
The packaged validation receipt records that prior run; it is not a package-only
recomputation. No follow-up cells were invalid, and no CPU quota-pressure flags
were raised. Low nonzero throttling was observed in some samples; the absence of
a pressure flag is not a claim of zero throttling.

The consolidated secondary analysis contains **3,736 paired metric comparisons**, including 312 primary-workload metric rows. Comparisons lacking five positive finite metric pairs were omitted; missing token-spacing observations were not treated as zero. A total of 208 primary estimates from the two smaller-model analyses were cross-checked against their previously published results.

The following constraints affect interpretation:

- **Warmed serving:** cold model loading, checkpoint creation time, restore duration and disk-heavy workloads are outside the serving-performance measurements.
- **Hardware assignment:** cross-condition launches could use different physical GPUs within a verified H100 pool. Each measurement retained one GPU throughout, and device identities were recorded. The earliest 0.6B measurements used a fixed-device policy before an approved pool policy was introduced.
- **Frontend resources:** Dynamo's frontend retained a one-CPU limit and showed quota pressure in earlier testing. Results describe that complete endpoint configuration rather than an unconstrained worker's maximum capacity.
- **Restore scope:** restore changes process state and adds a seccomp layer. Its effects cannot be attributed exclusively to io_uring.
- **Synthetic workloads:** cached text workloads and fixed token lengths do not establish results for every production request distribution or I/O pattern.
- **Tail precision:** low-volume p99 measurements outside the primary workload are flagged in the data. Secondary comparisons are exploratory and unadjusted for multiplicity.
- **Streaming measurements:** per-request token-spacing summaries and observed chunk gaps measure different aspects of delivery; missing gap observations are not zero.
- **Interrupted execution:** recovery idle periods affected two follow-up pairs; sensitivity checks retain their uncertainty. A failed native restore was excluded before measurement. Accepted performance samples do not estimate restore reliability.
- **Unmeasured outcomes:** the analysis does not establish model-output accuracy, whole-system energy cost, or capacity beyond concurrency 128.

## Conclusion

Blocking io_uring produced no consistent throughput regression across the tested vLLM, SGLang and Dynamo-vLLM configurations. Average request latency, TTFT, ITL and CPU cost per request likewise showed no consistent blocking penalty.

Checkpoint-restored serving with blocking enabled maintained throughput close to ordinary serving and showed no consistent average-performance or CPU-efficiency penalty. Tail metrics were more variable. The targeted Dynamo-vLLM follow-up did not establish a repeatable p99 penalty, but its uncertainty prevents a claim that restore-related latency degradation is below 3%.

The evidence supports limited throughput and average-performance impact for these warmed serving workloads. Tighter tail-latency guarantees, cold-start behavior and workloads with heavier I/O dependence remain unresolved.

## Reproducibility artifacts

The report package includes per-model analysis, consolidated secondary-metric comparisons, follow-up results, validation receipts and a file-integrity manifest. Detailed CPU provenance and per-run values are preserved in the analysis JSON files. Raw request exports and complete run directories are not included in the
package. The original continuation progress file is unavailable.

- Main sweeps: 540 accepted cells per model; five paired repetitions per comparison.
- Consolidated metrics: [CSV](analysis/all-model-paired-metrics.csv) and [JSON](analysis/all-model-paired-metrics.json).
- Targeted follow-up: [results](analysis/tail-results.json) and the
  [receipt of validation in the original archive](analysis/final-analysis-validation.json).
  The package omits both segments' `formal-progress.json` records and raw
  request exports, so it cannot regenerate the follow-up results or sensitivity
  tables. The [source record](analysis/tail/continuation-source.json) retains
  the earlier segment's progress hash and marks the continuation's original
  progress file and hash as unavailable. Its source integrity cannot be
  independently verified from this package.
- Model revisions and frozen execution versions:
  [0.6B](analysis/0.6b/formal-freeze.json),
  [8B](analysis/8b/formal-freeze.json),
  [27B](analysis/27b/formal-freeze.json) and
  [follow-up](analysis/tail/formal-freeze.json).

All reported percentages are rounded for presentation. Machine-readable artifacts retain full precision.
