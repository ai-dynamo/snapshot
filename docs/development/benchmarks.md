# Performance Benchmarks

We tested 22 configurations of vLLM, SGLang and TensorRT-LLM on one to eight B200 GPUs.
Restore was 8.1–35.9 times faster than cold start. Both measurements end when the engine
is ready, before inference. Read the [limitations](#limitations) before you compare
these results with another setup.

> [!TIP]
> See the [benchmark guide](benchmarks-guide.md) for setup and commands, and the
> [runner README](../../benchmarks/README.md) for the code layout.

During cold start, the engine loads weights, creates workers and sets up device memory.
It also compiles and tunes kernels, then captures CUDA graphs. Snapshot restores the
process with this work complete and the engine ready to serve requests.

## Benchmarking setup

| Property | Value |
|---|---|
| GPU | NVIDIA B200; 1, 2, 4 or 8 GPUs per workload |
| NVIDIA driver | 615.71.09 |
| Snapshot | v0.2.0-rc.2 |
| Engines | vLLM 0.31.0; SGLang 0.5.21; TensorRT-LLM 1.3.0rc24 |
| Model weights | 1.50–913.09 GB, already present on shared storage |
| Storage | VAST NFSv3 over TCP; 1 MiB reads/writes; `nconnect=32`, `spread_reads`, four server addresses |
| Compiler caches | Fresh directories for each cold start |
| Weight loading | vLLM: prefetch with eight threads and 16 MiB blocks; SGLang: checkpoint prefetch with four threads |
| Parallelism | TP: tensor parallelism. TEP8: tensor and expert parallelism across eight GPUs; exact settings are in each manifest |

## Limitations

- Cold starts and restores came from separate runs. Some runs used different nodes.
- Results apply to this B200 and VAST NFS setup. GPU, driver, storage and engine settings can change the times.
- Timing instrumentation adds work during cold start.

Weight size is the sum of the logical sizes of cached weight files. Checkpoint size is
the sum of the logical sizes of saved container files. It includes CPU state, GPU state
and the root filesystem archive. Both sizes use decimal GB. File size can differ from
GPU memory use and the disk space used.

Before capture, we asked each engine to release KV cache memory. For vLLM, model
weights are resident on the GPU at capture time.

## Measurement boundaries

**Cold start** begins at the process-start marker and ends when engine initialization
returns. This time excludes model download, pod scheduling, image pull and container
startup. We check text generation after initialization. That check is outside the
measured time.

**Snapshot** begins at the agent's external-restore start marker. It ends when the
application completes its wake and resume calls. The destination container already
exists. We then check inference and the saved process token. These checks are outside
the measured time.

**Checkpoint time** is the time the agent takes to capture the process. It excludes the
application's sleep or memory-release call before capture.

### Cold-start stages

| Stage | What is included |
|---|---|
| Startup | Imports, worker creation and runtime setup before model construction. |
| Model load | Model construction, weight reads and weight preparation, excluding separately timed nested phases. |
| Initialization | Memory profiling, attention/sampler initialization and remaining measured engine warmup. |
| Compilation, autotuning & graph capture | The combined measured time for kernel compilation, tuning and graph-capture scopes, including compilation inside those scopes. |

### Restore stages

| Stage | What is included |
|---|---|
| Agent setup / rootfs | The agent duration outside its CRIU and CUDA phase timers, including filesystem preparation and rootfs work. |
| CRIU restore | The measured CRIU phase that reconstructs the saved process state. |
| CUDA restore | The measured CUDA restoration phase, including multi-GPU reconstruction where applicable. |
| Wake | Time from agent completion through the application's wake and resume calls. |

## Results

All times are in seconds. Speedup is cold-start time divided by restore time.
Cold-start values use the mean when multiple samples are available.

### Cold start compared with Snapshot

| Model | Engine | Parallelism | Weights (GB) | Checkpoint (GB) | Checkpoint (s) | Cold start (s) | Restore (s) | Speedup |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 1.50 | 8.13 | 15.30 | 104.43 | 4.66 | 22.40× |
| Qwen3-0.6B | SGLang | TP1 | 1.50 | 9.42 | 20.71 | 87.08 | 3.59 | 24.26× |
| Qwen3-0.6B | TRT-LLM | TP1 | 1.50 | 8.04 | 20.07 | 121.26 | 8.15 | 14.88× |
| Qwen3-14B | vLLM | TP1 | 29.54 | 37.71 | 19.98 | 122.76 | 5.72 | 21.45× |
| Qwen3-14B | vLLM | TP2 | 29.54 | 48.29 | 38.37 | 139.63 | 6.19 | 22.55× |
| Qwen3-14B | SGLang | TP1 | 29.54 | 40.19 | 26.17 | 106.26 | 5.60 | 18.96× |
| Qwen3-14B | SGLang | TP2 | 29.54 | 51.93 | 33.88 | 124.72 | 7.45 | 16.75× |
| Qwen3-14B | TRT-LLM | TP1 | 29.54 | 36.38 | 23.72 | 127.02 | 12.50 | 10.16× |
| Qwen3-14B | TRT-LLM | TP2 | 29.54 | 47.97 | 42.90 | 158.66 | 15.99 | 9.92× |
| gpt-oss-120b | vLLM | TP1 | 65.25 | 81.56 | 44.37 | 172.80 | 10.09 | 17.12× |
| gpt-oss-120b | vLLM | TP4 | 65.25 | 119.69 | 133.01 | 155.66 | 11.62 | 13.40× |
| gpt-oss-120b | SGLang | TP1 | 65.25 | 172.32 | 55.63 | 268.96 | 15.94 | 16.87× |
| gpt-oss-120b | SGLang | TP4 | 65.25 | 693.89 | 174.20 | 240.17 | 29.64 | 8.10× |
| gpt-oss-120b | TRT-LLM | TP1 | 65.25 | 77.87 | 29.98 | 200.55 | 7.79 | 25.75× |
| gpt-oss-120b | TRT-LLM | TP4 | 65.25 | 111.34 | 85.29 | 223.96 | 18.58 | 12.05× |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 464.82 | 604.00 | 282.80 | 393.65 | 28.64 | 13.74× |
| GLM-5.3-NVFP4 | SGLang | TEP8 | 464.82 | 669.49 | 327.20 | 426.70 | 35.34 | 12.07× |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 464.82 | 628.30 | 231.33 | 363.90 | 33.55 | 10.85× |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 510.30 | 647.18 | 274.75 | 1110.65 | 30.96 | 35.87× |
| DeepSeek-V4.1-Flash | SGLang | TEP8 | 510.30 | 743.35 | 286.77 | 953.09 | 36.54 | 26.08× |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 913.09 | 996.53 | 193.84 | 908.48 | 39.56 | 22.96× |
| DeepSeek-V4-Pro-NVFP4 | SGLang | TEP8 | 913.09 | 1039.09 | 253.72 | 621.55 | 42.69 | 14.56× |

Figures 1–3 compare cold start with restore time for each engine.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/vllm-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and restore on vLLM." src="img/vllm-cold-start-vs-snapshot.png">
</picture>

*Figure 1. Cold start and restore on vLLM.*

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/sglang-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and restore on SGLang." src="img/sglang-cold-start-vs-snapshot.png">
</picture>

*Figure 2. Cold start and restore on SGLang.*

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/trtllm-cold-start-vs-snapshot-dark.png">
  <img alt="Cold start and restore on TensorRT-LLM." src="img/trtllm-cold-start-vs-snapshot.png">
</picture>

*Figure 3. Cold start and restore on TensorRT-LLM.*

### Where the cold start time goes

| Model | Engine | Parallelism | Startup | Model load | Initialization | Compilation, autotuning & graph capture | Total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 31.37 | 17.27 | 14.58 | 41.21 | 104.43 |
| Qwen3-0.6B | SGLang | TP1 | 33.74 | 11.67 | 8.41 | 33.26 | 87.08 |
| Qwen3-0.6B | TRT-LLM | TP1 | 25.79 | 3.79 | 40.64 | 51.04 | 121.26 |
| Qwen3-14B | vLLM | TP1 | 31.16 | 26.15 | 15.30 | 50.15 | 122.76 |
| Qwen3-14B | vLLM | TP2 | 42.18 | 18.96 | 15.29 | 63.20 | 139.63 |
| Qwen3-14B | SGLang | TP1 | 33.45 | 20.79 | 8.28 | 43.74 | 106.26 |
| Qwen3-14B | SGLang | TP2 | 47.62 | 18.93 | 8.29 | 49.88 | 124.72 |
| Qwen3-14B | TRT-LLM | TP1 | 25.35 | 11.54 | 39.36 | 50.77 | 127.02 |
| Qwen3-14B | TRT-LLM | TP2 | 27.42 | 8.88 | 70.62 | 51.74 | 158.66 |
| gpt-oss-120b | vLLM | TP1 | 35.15 | 38.69 | 10.49 | 88.47 | 172.80 |
| gpt-oss-120b | vLLM | TP4 | 49.91 | 24.71 | 12.13 | 68.91 | 155.66 |
| gpt-oss-120b | SGLang | TP1 | 42.54 | 34.44 | 0.36 | 191.62 | 268.96 |
| gpt-oss-120b | SGLang | TP4 | 57.97 | 49.06 | 1.33 | 131.81 | 240.17 |
| gpt-oss-120b | TRT-LLM | TP1 | 26.21 | 47.50 | 42.85 | 83.99 | 200.55 |
| gpt-oss-120b | TRT-LLM | TP4 | 34.28 | 42.72 | 22.34 | 124.62 | 223.96 |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 65.47 | 65.88 | 48.88 | 213.42 | 393.65 |
| GLM-5.3-NVFP4 | SGLang | TEP8 | 60.77 | 73.98 | 2.66 | 289.29 | 426.70 |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 31.58 | 90.39 | 120.43 | 121.50 | 363.90 |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 63.96 | 184.90 | 77.25 | 784.54 | 1110.65 |
| DeepSeek-V4.1-Flash | SGLang | TEP8 | 56.18 | 74.21 | 38.50 | 784.20 | 953.09 |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 64.42 | 95.14 | 64.87 | 684.05 | 908.48 |
| DeepSeek-V4-Pro-NVFP4 | SGLang | TEP8 | 58.80 | 87.68 | 42.55 | 432.52 | 621.55 |

Cold-start phases may not sum to the total because logging and instrumentation do not
capture every interval. Each run saves the raw timings.

### Where the restore time goes

| Model | Engine | Parallelism | Agent setup / rootfs | CRIU restore | CUDA restore | Wake | Total |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-0.6B | vLLM | TP1 | 2.30 | 1.36 | 0.78 | 0.22 | 4.66 |
| Qwen3-0.6B | SGLang | TP1 | 0.24 | 1.33 | 1.02 | 0.99 | 3.59 |
| Qwen3-0.6B | TRT-LLM | TP1 | 5.72 | 1.43 | 0.84 | 0.16 | 8.15 |
| Qwen3-14B | vLLM | TP1 | 0.13 | 1.58 | 3.16 | 0.85 | 5.72 |
| Qwen3-14B | vLLM | TP2 | 0.57 | 2.03 | 3.43 | 0.17 | 6.19 |
| Qwen3-14B | SGLang | TP1 | 0.24 | 1.35 | 3.67 | 0.34 | 5.60 |
| Qwen3-14B | SGLang | TP2 | 0.92 | 2.17 | 3.49 | 0.87 | 7.45 |
| Qwen3-14B | TRT-LLM | TP1 | 6.80 | 2.70 | 2.82 | 0.18 | 12.50 |
| Qwen3-14B | TRT-LLM | TP2 | 9.65 | 3.52 | 2.41 | 0.41 | 15.99 |
| gpt-oss-120b | vLLM | TP1 | 0.48 | 2.60 | 6.71 | 0.30 | 10.09 |
| gpt-oss-120b | vLLM | TP4 | 1.02 | 4.40 | 5.57 | 0.62 | 11.62 |
| gpt-oss-120b | SGLang | TP1 | 0.26 | 1.69 | 13.46 | 0.54 | 15.94 |
| gpt-oss-120b | SGLang | TP4 | 2.69 | 4.34 | 21.69 | 0.93 | 29.64 |
| gpt-oss-120b | TRT-LLM | TP1 | 0.28 | 1.51 | 5.74 | 0.26 | 7.79 |
| gpt-oss-120b | TRT-LLM | TP4 | 9.99 | 3.30 | 4.34 | 0.95 | 18.58 |
| GLM-5.3-NVFP4 | vLLM | TEP8 | 3.79 | 4.18 | 20.61 | 0.06 | 28.64 |
| GLM-5.3-NVFP4 | SGLang | TEP8 | 3.27 | 10.80 | 19.96 | 1.31 | 35.34 |
| GLM-5.3-NVFP4 | TRT-LLM | TEP8 | 9.81 | 5.32 | 18.17 | 0.25 | 33.55 |
| DeepSeek-V4.1-Flash | vLLM | TEP8 | 4.84 | 5.71 | 19.37 | 1.04 | 30.96 |
| DeepSeek-V4.1-Flash | SGLang | TEP8 | 4.34 | 9.77 | 21.40 | 1.03 | 36.54 |
| DeepSeek-V4-Pro-NVFP4 | vLLM | TEP8 | 5.44 | 4.73 | 28.98 | 0.42 | 39.56 |
| DeepSeek-V4-Pro-NVFP4 | SGLang | TEP8 | 3.13 | 10.13 | 28.60 | 0.83 | 42.69 |

Wake starts when the agent finishes and ends when the application completes its wake
and resume calls. CUDA restore includes multi-GPU reconstruction. Values are rounded
to two decimal places.

Figure 4 shows how much each stage contributes to vLLM restore.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="img/vllm-restore-stages-dark.png">
  <img alt="vLLM restore split into Agent setup / rootfs, CRIU restore, CUDA restore and Wake." src="img/vllm-restore-stages.png">
</picture>

*Figure 4. Restore on vLLM, split into the four reported stages.*

## Reading the results

### Startup work

Restore skips model loading, kernel compilation, autotuning and CUDA graph capture.
Compilation can run during tuning and graph capture, so the table groups these phases
in one column.

### Restore work and storage

CUDA restore takes 19.37–28.98 seconds for the eight-GPU vLLM models. For Qwen3-0.6B,
agent setup and CRIU take 3.66 of the 4.66-second restore.

Checkpoint size, storage throughput and parallel reads affect restore time. The
81.56 GB vLLM GPT-OSS TP1 checkpoint restores in 10.09 seconds. The 119.69 GB TP4
checkpoint restores in 11.62 seconds.

### SGLang model loading

Qwen3-14B TP1 loads in 20.79 seconds,
compared with 26.15 seconds on vLLM. GPT-OSS TP1 loads in 34.44 seconds on SGLang and
38.69 seconds on vLLM. At TP4, GPT-OSS loads in 49.06 seconds on SGLang and
24.71 seconds on vLLM. Model load includes weight reads and preparation.
