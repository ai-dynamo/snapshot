# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Add benchmark instrumentation and isolated caches to a guide pod."""

import json

from snapshot_benchmarks.engines.common import ROOT, set_env


def app_configmap(engine, namespace, name):
    data = {"app.py": (ROOT / "docs/guides" / engine.guide / "app.py").read_text()}
    data.update(
        {
            p.name: p.read_text()
            for p in (ROOT / "benchmarks/instrumentation").glob("*.py")
        }
    )
    return dict(
        apiVersion="v1",
        kind="ConfigMap",
        metadata=dict(name=name, namespace=namespace),
        data=data,
    )


def instrument_pod(pod, cfg, engine, model, run_id, *, restore=False):
    if engine.name == "vllm" and model.ep not in (1, model.tp):
        raise ValueError("vLLM expert parallelism must be 1 or TP")
    spec = pod["spec"]
    spec["restartPolicy"] = "Never"
    spec["nodeSelector"]["kubernetes.io/hostname"] = cfg.node
    pod["metadata"].setdefault("labels", {})["snapshot-benchmark"] = run_id
    if model.tp > 1:
        pod["metadata"].setdefault("annotations", {})[
            "nvidia.com/snapshot-multi-gpu"
        ] = "enabled"
    main = next(c for c in spec["containers"] if c["name"] == engine.container_name)
    main["resources"]["limits"]["nvidia.com/gpu"] = str(model.tp)
    if "nvidia.com/gpu" in main["resources"].get("requests", {}):
        main["resources"]["requests"]["nvidia.com/gpu"] = str(model.tp)
    prefix = []
    if not restore and model.tp > 1:
        command = main["command"]
        prefix = command[: command.index("--") + 1]
    # Older operators do not replace the entrypoint with a restore proxy.
    # Keep the destination idle while the agent restores the saved process.
    main["command"] = (
        ["sleep", "infinity"]
        if restore
        else prefix + ["python3", "/snapshot-app/launch.py"]
    )
    values = dict(
        HF_HOME="/hf-cache",
        HF_HUB_OFFLINE="1",
        HF_HUB_DISABLE_XET="1",
        HF_MODULES_CACHE="/compile-cache/hf-modules",
        PYTHONPATH="/snapshot-app",
        PYTHONUNBUFFERED="1",
        XDG_CACHE_HOME="/compile-cache/xdg",
        TORCHINDUCTOR_CACHE_DIR="/compile-cache/torchinductor",
        CUDA_CACHE_PATH="/compile-cache/cuda",
        DG_JIT_CACHE_DIR="/compile-cache/deepgemm",
        FLASHINFER_WORKSPACE_BASE="/compile-cache/flashinfer",
        TRITON_CACHE_DIR="/snapshot-triton-cache"
        if engine.name == "sglang"
        else "/compile-cache/triton",
        TORCH_EXTENSIONS_DIR="/compile-cache/torch-extensions",
        STARTUP_SPAN_DIRECTORY="/startup-spans",
        SNAPSHOT_TENSOR_PARALLEL_SIZE=str(model.tp),
    )
    extra = dict(model.engine_args)
    if engine.name == "vllm":
        values.update(
            VLLM_CACHE_ROOT="/compile-cache/vllm",
            VLLM_FLASHINFER_AUTOTUNE_CACHE_DIR="/compile-cache/vllm/flashinfer_autotune",
            PYTORCH_ALLOC_CONF="pinned_max_round_threshold_mb:1,pinned_max_cached_size_mb:1",
        )
        extra["enable_expert_parallel"] = model.ep > 1
        extra.update(model=model.hf_id_or_path, tensor_parallel_size=model.tp)
    elif engine.name == "sglang":
        values.update(
            SGLANG_CACHE_DIR="/compile-cache/sglang",
            SGLANG_JIT_CACHE_DIR="/compile-cache/sglang-jit",
        )
        extra["ep_size"] = model.ep
        extra.update(model_path=model.hf_id_or_path, tp_size=model.tp)
    else:
        extra["moe_expert_parallel_size"] = model.ep
        values.update(
            OMPI_MCA_pml="ob1",
            OMPI_MCA_btl="tcp,self",
            # Loaded/deleted extension files must survive independently per restore.
            FLASHINFER_WORKSPACE_BASE="/snapshot-flashinfer-cache",
        )
        extra.update(model=model.hf_id_or_path, tensor_parallel_size=model.tp)
    main["args"] = [] if restore else [json.dumps(extra)]
    for key, value in values.items():
        set_env(main, key, value)
    mounts = main["volumeMounts"]
    mounts[:] = [
        m
        for m in mounts
        if m["name"] not in ("model-cache", "bench-cache", "shm", "startup-spans")
    ]
    mounts.extend(
        [
            dict(name="model-cache", mountPath="/hf-cache", readOnly=True),
            dict(
                name="bench-cache",
                mountPath="/compile-cache",
                subPath=run_id + "/compile-cache",
            ),
            dict(
                name="bench-cache",
                mountPath="/root/.cache/flashinfer",
                subPath=run_id + "/flashinfer",
            ),
            dict(name="shm", mountPath="/dev/shm"),
            dict(name="startup-spans", mountPath="/startup-spans"),
        ]
    )
    cache_dirs = ["compile-cache", "flashinfer"]
    if engine.name == "trtllm":
        mounts.append(
            dict(
                name="bench-cache",
                mountPath="/tmp/torchinductor_root",
                subPath=run_id + "/torchinductor-root",
            )
        )
        cache_dirs.append("torchinductor-root")
    volumes = spec["volumes"]
    volumes[:] = [
        v
        for v in volumes
        if v["name"] not in ("model-cache", "bench-cache", "shm", "startup-spans")
    ]
    volumes.extend(
        [
            dict(
                name="model-cache",
                persistentVolumeClaim=dict(claimName=cfg.model_pvc, readOnly=True),
            ),
            dict(
                name="bench-cache", persistentVolumeClaim=dict(claimName=cfg.work_pvc)
            ),
            dict(
                name="shm",
                emptyDir=dict(
                    medium="Memory", sizeLimit="16Gi" if model.tp > 1 else "1Gi"
                ),
            ),
            # Frequent timing events must not measure network-storage writes.
            dict(
                name="startup-spans",
                emptyDir=dict(medium="Memory", sizeLimit="2Gi"),
            ),
        ]
    )
    next(v for v in volumes if v["name"] == "app")["configMap"]["name"] = (
        "bench-app-" + run_id
    )
    # The guide's model-download init container would invalidate an offline measurement.
    spec["initContainers"] = [
        c for c in spec.get("initContainers", []) if c["name"] != "model-cache"
    ]
    if not restore:
        script = (
            "from pathlib import Path; "
            f"root=Path('/work')/{run_id!r}; "
            f"[(root/p).mkdir(parents=True, exist_ok=False) for p in {cache_dirs!r}]"
        )
        spec["initContainers"].append(
            dict(
                name="fresh-cache",
                image=main["image"],
                imagePullPolicy=main["imagePullPolicy"],
                command=["python3", "-c", script],
                volumeMounts=[dict(name="bench-cache", mountPath="/work")],
            )
        )
    return pod
