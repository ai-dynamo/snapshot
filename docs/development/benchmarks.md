# Performance Benchmarks

These benchmarks compare cold start with Snapshot restore for 22 configurations of vLLM, SGLang and TensorRT-LLM on one to eight B200 GPUs. Both timings end when the engine is ready, before inference. Restore was 9.9–77.6 times faster in these runs. See [Limitations](#limitations) before comparing the results with another setup.

> [!TIP]
> See the [benchmark guide](benchmarks-guide.md) for setup and commands, and the
> [runner README](../../benchmarks/README.md) for the code layout.

Cold start loads weights, creates workers, initializes device memory, compiles and tunes kernels, and captures CUDA graphs. Snapshot restores a process in a warm state, ready for serving.

## Benchmarking setup

| Property | Value |
|---|---|
| GPU | NVIDIA B200; 1, 2, 4 or 8 GPUs per workload |
| NVIDIA driver | 615.71.09 |
| Snapshot | v0.2.0-rc.2 |
| Engines | vLLM 0.31.0; SGLang 0.5.21; TensorRT-LLM 1.3.0rc24 |
| Model weights | 1.50–913.09 GB, already present on shared storage |
| Storage | VAST NFSv3 over TCP; 1 MiB reads/writes; `nconnect=32`, `spread_reads`, four server addresses |
| Compiler caches | Fresh compiler-cache directories for each cold start; OS and storage cache state varied on shared infrastructure |
| Parallelism | TP: tensor parallelism. TEP8: tensor and expert parallelism across eight GPUs; exact settings are in each manifest |
| Restore samples | Three per configuration; the tables use the first restore |

## Limitations

- Cold starts and restores came from separate runs and did not always use the same node.
- Compiler caches started empty; OS and storage cache state varied on the shared infrastructure.
- Results apply to this B200 and VAST NFS setup. GPU, driver, storage and engine settings can change the times.
- The published cold starts include instrumentation overhead. They have not been rerun after fixes that narrowed constructor timers and disabled unused import profiling.
- The five TRT-LLM Qwen and GPT-OSS cold starts allowed FlashInfer artifact downloads. The [offline follow-up](#flashinfer-artifact-downloads) found differences of up to 33 seconds; the tables retain the original measurements.

Weights are the logical bytes in the cached model weight files, using decimal GB. Checkpoint size is the sum of logical file sizes in the captured container's artifact. This includes CPU state, GPU state and the root filesystem archive. Logical size can differ from GPU memory use and allocated disk space.

Before capture, each engine was asked to release memory it no longer needs, such as the KV cache. In the SGLang GPT-OSS runs, KV allocations were still held at capture time. Those checkpoints include the retained memory, which adds work during restore. Those rows are marked ¹. Memory release has not been checked individually for the larger SGLang models (²).

## Measurement boundaries

**Cold start** begins at the workload's process-start marker and ends when engine initialization returns. Model download, pod scheduling, image pull and container startup are outside this interval. A generation check follows initialization and is excluded from the reported time.

**Snapshot** begins at the agent's external-restore start marker and ends after the application's wake and resume calls complete. The destination container already exists. Generation checks run afterward to verify that inference works and that the process token survived restore; their time is excluded.

**Checkpoint time** is the agent's capture duration. It excludes the preceding application sleep or memory-release call.

### Cold-start stages

| Stage | What is included |
|---|---|
| Startup | Imports, worker creation and runtime setup before model construction; some early kernel JIT also occurs here. |
| Model load | Model construction, weight reads and weight preparation, excluding separately timed nested phases. |
| Initialization | Memory profiling, attention/sampler initialization and remaining measured engine warmup. |
| Compilation, autotuning & graph capture | The combined measured time for kernel compilation, tuning and graph-capture scopes, including compilation inside those scopes. |

### Restore stages

| Stage | What is included |
|---|---|
| Agent setup / rootfs | The agent duration outside its CRIU and CUDA phase timers, including filesystem preparation and rootfs work. |
| CRIU restore | The measured CRIU phase that reconstructs the saved process state. |
| CUDA restore | The measured CUDA restoration phase, including multi-GPU reconstruction where applicable. |
| Wake / handoff | Time from agent completion through the application's wake and resume calls, including the polling handoff. |

## Results

All times are seconds. Speedup is cold-start time divided by the first restore time; configurations with multiple cold-start samples use their mean. Engine settings, including weight loading, differ between rows, so compare engines with care.

### Cold start compared with Snapshot

| Model | Engine | Parallelism | Weights (GB) | Checkpoint (GB) | Checkpoint (s) | Cold start (s) | Snapshot (s) | Speedup |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 1.50 | 8.13 | 15.30 | 104.43 | 4.66 | 22.40× |
| Qwen3-0.6B | SGLang | TP1 | 1.50 | 9.63 | 21.73 | 98.28 | 5.17 | 19.02× |
| Qwen3-0.6B | TRT-LLM | TP1 | 1.50 | 8.04 | 20.07 | 121.26 | 8.15 | 14.88× |
| Qwen3-14B | vLLM | TP1 | 29.54 | 37.71 | 19.98 | 122.76 | 5.72 | 21.45× |
| Qwen3-14B | vLLM | TP2 | 29.54 | 48.29 | 38.37 | 139.63 | 6.19 | 22.55× |
| Qwen3-14B | SGLang | TP1 | 29.54 | 40.11 | 24.63 | 423.80 | 5.46 | 77.61× |
| Qwen3-14B | SGLang | TP2 | 29.54 | 51.79 | 37.25 | 364.21 | 6.36 | 57.30× |
| Qwen3-14B | TRT-LLM | TP1 | 29.54 | 36.38 | 23.72 | 127.02 | 12.50 | 10.16× |
| Qwen3-14B | TRT-LLM | TP2 | 29.54 | 47.97 | 42.90 | 158.66 | 15.99 | 9.92× |
| gpt-oss-120b | vLLM | TP1 | 65.25 | 81.56 | 44.37 | 172.80 | 10.09 | 17.12× |
| gpt-oss-120b | vLLM | TP4 | 65.25 | 119.69 | 133.01 | 155.66 | 11.62 | 13.40× |
| gpt-oss-120b | SGLang¹ | TP1 | 65.25 | 173.32 | 56.56 | 964.08 | 15.73 | 61.28× |
| gpt-oss-120b | SGLang¹ | TP4 | 65.25 | 698.89 | 169.66 | 988.18 | 32.51 | 30.40× |
| gpt-oss-120b | TRT-LLM | TP1 | 65.25 | 77.87 | 29.98 | 200.55 | 7.79 | 25.75× |
| gpt-oss-120b | TRT-LLM | TP4 | 65.25 | 111.34 | 85.29 | 223.96 | 18.58 | 12.05× |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 464.82 | 604.00 | 282.80 | 393.65 | 28.64 | 13.74× |
| GLM-5.3-NVFP4 | SGLang² | TEP8 | 464.82 | 671.04 | 326.92 | 895.85 | 36.24 | 24.72× |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 464.82 | 628.30 | 231.33 | 363.90 | 33.55 | 10.85× |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 510.30 | 647.18 | 274.75 | 1110.65 | 30.96 | 35.87× |
| DeepSeek-V4.1-Flash | SGLang² | TEP8 | 510.30 | 743.93 | 300.78 | 1474.44 | 38.28 | 38.52× |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 913.09 | 996.53 | 193.84 | 908.48 | 39.56 | 22.96× |
| DeepSeek-V4-Pro-NVFP4 | SGLang² | TEP8 | 913.09 | 1042.12 | 185.61 | 1443.23 | 44.43 | 32.48× |

¹ SGLang GPT-OSS: the v0.5.21 image still held KV allocations after the memory-release call. Checkpoint sizes and restore times include that memory.

² Large SGLang models: whether memory is fully released before capture has not been checked yet.

Figures 1–3 compare cold start with the first restore for each engine.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/vllm-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and first restore on vLLM." src="img/vllm-cold-start-vs-snapshot.png">
</picture>

*Figure 1. Cold start and first restore on vLLM.*

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/sglang-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and first restore on SGLang." src="img/sglang-cold-start-vs-snapshot.png">
</picture>

*Figure 2. Cold start and first restore on SGLang.*

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/trtllm-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and first restore on TensorRT-LLM." src="img/trtllm-cold-start-vs-snapshot.png">
</picture>

*Figure 3. Cold start and first restore on TensorRT-LLM.*

### Where the cold start time goes

| Model | Engine | Parallelism | Startup | Model load | Initialization | Compilation, autotuning & graph capture | Total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 31.37 | 17.27 | 14.58 | 41.21 | 104.43 |
| Qwen3-0.6B | SGLang | TP1 | 33.68 | 21.39 | 8.40 | 34.81 | 98.28 |
| Qwen3-0.6B | TRT-LLM | TP1 | 25.79 | 3.79 | 40.64 | 51.04 | 121.26 |
| Qwen3-14B | vLLM | TP1 | 31.16 | 26.15 | 15.30 | 50.15 | 122.76 |
| Qwen3-14B | vLLM | TP2 | 42.18 | 18.96 | 15.29 | 63.20 | 139.63 |
| Qwen3-14B | SGLang | TP1 | 33.47 | 337.49 | 8.28 | 44.56 | 423.80 |
| Qwen3-14B | SGLang | TP2 | 47.03 | 257.12 | 8.19 | 51.87 | 364.21 |
| Qwen3-14B | TRT-LLM | TP1 | 25.35 | 11.54 | 39.36 | 50.77 | 127.02 |
| Qwen3-14B | TRT-LLM | TP2 | 27.42 | 8.88 | 70.62 | 51.74 | 158.66 |
| gpt-oss-120b | vLLM | TP1 | 35.15 | 38.69 | 10.49 | 88.47 | 172.80 |
| gpt-oss-120b | vLLM | TP4 | 49.91 | 24.71 | 12.13 | 68.91 | 155.66 |
| gpt-oss-120b | SGLang¹ | TP1 | 41.97 | 729.12 | 0.64 | 192.35 | 964.08 |
| gpt-oss-120b | SGLang¹ | TP4 | 58.24 | 803.03 | 1.55 | 125.36 | 988.18 |
| gpt-oss-120b | TRT-LLM | TP1 | 26.21 | 47.50 | 42.85 | 83.99 | 200.55 |
| gpt-oss-120b | TRT-LLM | TP4 | 34.28 | 42.72 | 22.34 | 124.62 | 223.96 |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 65.47 | 65.88 | 48.88 | 213.42 | 393.65 |
| GLM-5.3-NVFP4 | SGLang² | TEP8 | 61.80 | 538.18 | 2.61 | 293.26 | 895.85 |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 31.58 | 90.39 | 120.43 | 121.50 | 363.90 |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 63.96 | 184.90 | 77.25 | 784.54 | 1110.65 |
| DeepSeek-V4.1-Flash | SGLang² | TEP8 | 57.08 | 544.45 | 34.61 | 838.30 | 1474.44 |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 64.42 | 95.14 | 64.87 | 684.05 | 908.48 |
| DeepSeek-V4-Pro-NVFP4 | SGLang² | TEP8 | 59.41 | 913.59 | 41.59 | 428.64 | 1443.23 |

These columns divide elapsed time between phases. They count overlapping work across GPU workers once. Small untimed intervals are spread proportionally across the columns. These additions are estimates. The runner saves raw timings and gaps in each new run's local evidence. Some kernel JIT occurs during Startup and is included there.

### Where the restore time goes

| Model | Engine | Parallelism | Agent setup / rootfs | CRIU restore | CUDA restore | Wake / handoff | Total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 2.30 | 1.36 | 0.78 | 0.22 | 4.66 |
| Qwen3-0.6B | SGLang | TP1 | 1.44 | 2.71 | 0.89 | 0.12 | 5.17 |
| Qwen3-0.6B | TRT-LLM | TP1 | 5.72 | 1.43 | 0.84 | 0.16 | 8.15 |
| Qwen3-14B | vLLM | TP1 | 0.13 | 1.58 | 3.16 | 0.85 | 5.72 |
| Qwen3-14B | vLLM | TP2 | 0.57 | 2.03 | 3.43 | 0.17 | 6.19 |
| Qwen3-14B | SGLang | TP1 | 0.14 | 1.30 | 3.10 | 0.92 | 5.46 |
| Qwen3-14B | SGLang | TP2 | 0.44 | 2.17 | 2.89 | 0.86 | 6.36 |
| Qwen3-14B | TRT-LLM | TP1 | 6.80 | 2.70 | 2.82 | 0.18 | 12.50 |
| Qwen3-14B | TRT-LLM | TP2 | 9.65 | 3.52 | 2.41 | 0.41 | 15.99 |
| gpt-oss-120b | vLLM | TP1 | 0.48 | 2.60 | 6.71 | 0.30 | 10.09 |
| gpt-oss-120b | vLLM | TP4 | 1.02 | 4.40 | 5.57 | 0.62 | 11.62 |
| gpt-oss-120b | SGLang¹ | TP1 | 0.13 | 1.60 | 13.58 | 0.42 | 15.73 |
| gpt-oss-120b | SGLang¹ | TP4 | 4.58 | 5.16 | 21.96 | 0.81 | 32.51 |
| gpt-oss-120b | TRT-LLM | TP1 | 0.28 | 1.51 | 5.74 | 0.26 | 7.79 |
| gpt-oss-120b | TRT-LLM | TP4 | 9.99 | 3.30 | 4.34 | 0.95 | 18.58 |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 3.79 | 4.18 | 20.61 | 0.06 | 28.64 |
| GLM-5.3-NVFP4 | SGLang² | TEP8 | 6.11 | 8.84 | 20.86 | 0.43 | 36.24 |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 9.81 | 5.32 | 18.17 | 0.25 | 33.55 |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 4.84 | 5.71 | 19.37 | 1.04 | 30.96 |
| DeepSeek-V4.1-Flash | SGLang² | TEP8 | 7.08 | 8.96 | 21.74 | 0.49 | 38.28 |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 5.44 | 4.73 | 28.98 | 0.42 | 39.56 |
| DeepSeek-V4-Pro-NVFP4 | SGLang² | TEP8 | 6.36 | 8.64 | 29.12 | 0.31 | 44.43 |

Every restore row uses attempt 1, including all its phase values. Wake / handoff includes the interval from agent completion through the application wake and resume calls. The CUDA/CRIU phases include GPU reconstruction and remapping. These operations have no separate timers here. Rounding can change a displayed sum by 0.01 seconds.

Figure 4 shows how much each stage contributes to the first vLLM restore.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/vllm-restore-stages-dark.png">
  <img alt="First vLLM restore split into Agent setup / rootfs, CRIU restore, CUDA restore and Wake / handoff." src="img/vllm-restore-stages.png">
</picture>

*Figure 4. First restore on vLLM, split into the four reported stages.*

## Reading the results

### Startup work

Snapshot skips the measured compilation, autotuning and graph-capture work. These phases share one column because kernel compilation can occur inside tuning and graph capture, and the engines expose different timers.

### Checkpoint size

Weight precision, parallelism and retained memory affect checkpoint size. SGLang's GPT-OSS TP4 checkpoint is 698.89 GB for 65.25 GB of weights: KV allocations remained after the release call. Its 32.51-second restore includes that retained memory.

### Restore work and storage

For the three TEP8 vLLM configurations, CUDA restore takes 19.37–28.98 seconds of the 28.64–39.56-second total. It is the largest stage in each row. For Qwen3-0.6B, agent setup and CRIU together take 3.66 of 4.66 seconds.

Checkpoint size affects how much data must be read, but it does not determine restore time alone. For example, the 81.56 GB vLLM GPT-OSS TP1 checkpoint restores in 10.09 seconds; the larger 119.69 GB TP4 checkpoint restores in 11.62 seconds. Parallel reads, CPU/GPU state, rootfs work and cache state also affect the total. Storage throughput limits the read stages, so another backend can change these results.

### SGLang model loading

SGLang Qwen3-14B TP1 spends 337.49 seconds in Model load, compared with 26.15 seconds for vLLM. GPT-OSS TP1 takes 729.12 versus 38.69 seconds. Model load includes weight preparation as well as reads, and the engines used different loading settings. These timings do not isolate the cause of that difference; they should not be read as a controlled engine comparison.

### Repeat restores

The tables use the first restore; the dataset also contains attempts two and three. Later attempts can be faster. Cache isolation is limited on shared infrastructure, so even the first attempt may read cached data. Direct I/O on one path does not bypass caches in every restore stage.

### FlashInfer artifact downloads

The published TRT-LLM cold starts allowed FlashInfer kernel artifact downloads.
A follow-up used the same pinned engine image with 11 pre-staged, checksum-verified
artifacts, `FLASHINFER_NO_DOWNLOAD=1` and a separate read-only
`FLASHINFER_CUBIN_DIR`. Generated JIT/autotune caches started empty; OS and storage
cache state varied on the shared infrastructure.

| Model | Parallelism | Published cold start (s) | Pre-staged artifacts (s) | Difference (s) |
| --- | --- | --- | --- | --- |
| Qwen3-0.6B | TP1 | 121.26 | 117.94 | -3.32 |
| Qwen3-14B | TP1 | 127.02 | 124.14 | -2.89 |
| Qwen3-14B | TP2 | 158.66 | 130.22 | -28.44 |
| gpt-oss-120b | TP1 | 200.55 | 198.10 | -2.45 |
| gpt-oss-120b | TP4 | 223.96 | 191.33 | -32.63 |

The multi-GPU differences were mainly in attention warmup. Logged fetch windows
covered only 2.81 seconds on Qwen3-14B TP2, so the full difference cannot be
attributed to downloads. Original-download controls took 158.39 seconds for that
configuration and 221.88 seconds for GPT-OSS TP4. The first offline GPT-OSS TP4
attempt failed output validation and is excluded; the table uses its successful
repeat. All five selected configurations passed output checks, capture and three
restores. The published tables retain the original measurements.
