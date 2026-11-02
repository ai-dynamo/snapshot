# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CLI entry point: `python -m snapshot_benchmarks <command>`.

See docs/development/benchmarks-guide.md for the full walkthrough.
"""

from __future__ import annotations

import argparse
import dataclasses
import uuid
import json
import sys
from pathlib import Path
from typing import Any

import yaml

from snapshot_e2e import k8s

from snapshot_benchmarks import metadata, report, results
from snapshot_benchmarks.engines import ModelSpec
from snapshot_benchmarks.engines.vllm import VLLMEngine
from snapshot_benchmarks.engines.sglang import SGLangEngine
from snapshot_benchmarks.engines.trtllm import TRTLLMEngine
from snapshot_benchmarks.workload import app_configmap, instrument_pod
from snapshot_benchmarks.measurement import save
from snapshot_benchmarks import run
from snapshot_benchmarks.run import BenchmarkConfig, CleanupError, run_benchmark

ENGINES = {"vllm": VLLMEngine(), "sglang": SGLangEngine(), "trtllm": TRTLLMEngine()}


def _parse_toleration(value: str) -> dict[str, str]:
    """Parses `key=value:effect` (e.g. `nvidia.com/gpu=true:NoSchedule`) into
    a Kubernetes toleration dict. Needed on clusters whose GPU nodes carry a
    scheduling taint the vLLM guide's own deployment YAML doesn't know
    about -- see docs/development/benchmarks-guide.md."""
    try:
        key_value, effect = value.rsplit(":", 1)
        key, val = key_value.split("=", 1)
    except ValueError:
        raise argparse.ArgumentTypeError(
            f"invalid --toleration {value!r}, expected key=value:effect"
        ) from None
    return {"key": key, "operator": "Equal", "value": val, "effect": effect}


def _add_cluster_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--context", required=True, help="Explicit Kubernetes context")
    parser.add_argument(
        "--namespace",
        default=None,
        help="Workload namespace (source/restore pods, PodSnapshot). "
        "Defaults to SNAPSHOT_E2E_TEST_NAMESPACE / E2EConfig.from_env().",
    )
    parser.add_argument(
        "--snapshot-namespace",
        default=None,
        help="Namespace the Snapshot operator/agent run in. Defaults to the "
        "same value as --namespace (matches e2e/'s convention); pass this "
        "explicitly if your install keeps them separate (e.g. 'snapshot' vs. "
        "'default', as this repo's own manual setup does).",
    )
    parser.add_argument(
        "--release",
        default=None,
        help="Helm release name. Defaults to E2EConfig.from_env().",
    )
    parser.add_argument(
        "--storage-type", choices=["pvc", "s3"], default="pvc",
        help="Checkpoint backend of the installed Snapshot release.",
    )
    parser.add_argument(
        "--pvc-name",
        default=None,
        help="Checkpoint PVC name. Defaults to E2EConfig.from_env().",
    )


def _benchmark_config(args: argparse.Namespace) -> BenchmarkConfig:
    base = k8s.E2EConfig.from_env()
    namespace = args.namespace or base.namespace
    return BenchmarkConfig(
        workload_namespace=namespace,
        snapshot_namespace=args.snapshot_namespace or namespace,
        release=args.release or base.release,
        pvc_name=args.pvc_name or base.pvc_name,
        kubeconfig=base.kubeconfig,
        storage_type=args.storage_type,
        context=args.context,
        node=getattr(args, "node", None),
        model_pvc=getattr(args, "model_pvc", None),
        work_pvc=getattr(args, "work_pvc", None),
        drop_page_cache=not getattr(args, "no_drop_page_cache", False),
        detailed_timing=getattr(args, "detailed_timing", False),
        restore_count=getattr(args, "restore_count", 3),
    )


def _stringify_env(label: str, env: dict[str, Any] | None) -> dict[str, str]:
    """Coerces scalar `env` values (e.g. an unquoted `0.75` or `2048` in YAML,
    parsed by PyYAML as a float/int, not a string) to `str`, since pod env var
    values are always strings anyway. Raises a clear, load-time error for
    anything that isn't a plain scalar (list/dict/None) instead of letting a
    malformed value reach `create_pod` and fail deep inside a Kubernetes API
    JSON-unmarshal error. Keys must already be plain strings (e.g. an unquoted
    numeric or boolean-looking key in YAML would otherwise reach `_set_env` as
    a non-string env var name) -- rejected outright, not coerced, since there's
    no sensible env var name to coerce a non-string key into."""
    result: dict[str, str] = {}
    for key, value in (env or {}).items():
        if not isinstance(key, str):
            raise ValueError(
                f"model {label!r}: env key {key!r} must be a string, got "
                f"{type(key).__name__} -- quote it in models.yaml"
            )
        if isinstance(value, bool) or not isinstance(value, (str, int, float)):
            raise ValueError(
                f"model {label!r}: env[{key!r}] must be a plain string/number, got "
                f"{type(value).__name__} ({value!r}) -- quote it in models.yaml"
            )
        result[key] = str(value)
    return result


def _load_models(path: Path) -> list[ModelSpec]:
    data = yaml.safe_load(path.read_text())
    return [
        ModelSpec(
            label=entry["label"],
            hf_id_or_path=entry["hf_id_or_path"],
            env=_stringify_env(entry["label"], entry.get("env")),
            tp=entry.get("tp", 1),
            ep=entry.get("ep", 1),
            engine_args=entry.get("engine_args", {}),
        )
        for entry in data["models"]
    ]


def cmd_metadata(args: argparse.Namespace) -> int:
    cfg = _benchmark_config(args)
    cfg.configure()
    env = metadata.collect_environment(
        pvc_namespace=cfg.snapshot_namespace, pvc_name=cfg.pvc_name,
        storage_type=cfg.storage_type, release=cfg.release,
    )
    print(json.dumps(_to_json(env), indent=2))
    return 0


def _to_json(dataclass_instance) -> dict:
    import dataclasses

    return dataclasses.asdict(dataclass_instance)


def _setup(args: argparse.Namespace) -> tuple[BenchmarkConfig, Any]:
    cfg = _benchmark_config(args)
    return cfg, ENGINES[args.engine]


def cmd_run(args: argparse.Namespace) -> int:
    cfg, engine = _setup(args)
    models = (
        {m.label: m for m in _load_models(Path(args.models))} if not args.model else {}
    )
    if args.model:
        model = ModelSpec(
            label=args.model_label or Path(args.model).name,
            hf_id_or_path=args.model,
        )
    elif args.model_label not in models:
        print(
            f"error: {args.model_label!r} not found in {args.models}", file=sys.stderr
        )
        print(f"available: {sorted(models)}", file=sys.stderr)
        return 2
    else:
        model = models[args.model_label]
    out_dir = results.invocation_dir(Path(args.output_dir), git_sha=run._git_sha())
    result = _run_one(cfg, engine, model, args, out_dir)
    if result is None:
        return 0
    path = results.write_result(result, out_dir)
    print(f"wrote {path}")
    return 0


def _run_one(
    cfg: BenchmarkConfig, engine, model, args: argparse.Namespace, out_dir: Path
) -> run.RunResult | None:
    extra = json.loads(args.engine_args)
    if not isinstance(extra, dict):
        raise ValueError("engine-args must be a JSON object")
    model = dataclasses.replace(
        model,
        tp=args.tp if args.tp is not None else model.tp,
        ep=args.ep if args.ep is not None else model.ep,
        engine_args={**model.engine_args, **extra},
    )
    if args.dry_run:
        run_id = uuid.uuid4().hex[:12]
        folder = out_dir / run_id
        app = app_configmap(engine, cfg.workload_namespace, "bench-app-" + run_id)
        save(folder / "app.json", app)
        for role in ("source", "restore"):
            build = (
                engine.build_source_pod
                if role == "source"
                else engine.build_restore_pod
            )
            kwargs = dict(
                name="bench-" + role + "-" + run_id,
                namespace=cfg.workload_namespace,
                image=args.image,
                model=model,
                image_pull_policy=args.image_pull_policy,
                tolerations=args.toleration,
            )
            if role == "restore":
                kwargs["snapshot_name"] = "bench-snapshot-" + run_id
            pod = instrument_pod(
                build(**kwargs), cfg, engine, model, run_id, restore=role == "restore"
            )
            save(folder / (role + ".json"), pod)
        print(f"Rendered guide-based workload into {folder}; no Kubernetes calls")
        return None
    return run_benchmark(
        cfg,
        engine,
        model,
        image=args.image,
        image_pull_policy=args.image_pull_policy,
        tolerations=args.toleration,
        mode=args.mode,
        keep=args.keep,
        timeout=args.timeout,
        evidence_dir=out_dir,
    )


def cmd_sweep(args: argparse.Namespace) -> int:
    cfg, engine = _setup(args)
    models = _load_models(Path(args.models))

    out_dir = results.invocation_dir(Path(args.output_dir), git_sha=run._git_sha())
    failures: list[str] = []
    for model in models:
        print(f"=== {model.label} ===")
        try:
            result = _run_one(cfg, engine, model, args, out_dir)
        except CleanupError as exc:
            # Cleanup itself failed -- a pod, PodSnapshot, or
            # PodSnapshotContent from this model may still be on the cluster.
            # Unlike an ordinary per-model failure, it is never safe to
            # continue the sweep from here: the next model could schedule
            # onto a GPU still held by the leaked resource, or trip over
            # stale checkpoint state. Always stop, regardless of --fail-fast.
            print(f"error: {model.label} cleanup failed: {exc}", file=sys.stderr)
            failures.append(model.label)
            print(
                "aborting sweep: cluster state may be contaminated, inspect and clean up manually",
                file=sys.stderr,
            )
            break
        except Exception as exc:  # noqa: BLE001 - recorded per-model, sweep continues
            print(f"error: {model.label} failed: {exc}", file=sys.stderr)
            failures.append(model.label)
            if args.fail_fast:
                raise
            continue
        if result is None:
            continue
        path = results.write_result(result, out_dir)
        print(f"wrote {path}")

    if failures:
        print(
            f"sweep finished with {len(failures)} failure(s): {failures}",
            file=sys.stderr,
        )
        return 1
    return 0


def cmd_report(args: argparse.Namespace) -> int:
    text = report.render(results.load_results(Path(args.results_dir)))
    if args.out:
        Path(args.out).write_text(text)
        print(f"wrote {args.out}")
    else:
        print(text)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m snapshot_benchmarks")
    subparsers = parser.add_subparsers(dest="command", required=True)

    metadata_parser = subparsers.add_parser(
        "metadata", help="Print the environment bundle for the current cluster context."
    )
    _add_cluster_args(metadata_parser)
    metadata_parser.set_defaults(func=cmd_metadata)

    run_parser = subparsers.add_parser("run", help="Run one model against one engine.")
    _add_cluster_args(run_parser)
    run_parser.add_argument("--engine", choices=sorted(ENGINES), default="vllm")
    run_parser.add_argument(
        "--model-label", help='e.g. "Qwen3 0.6B", must match models.yaml'
    )
    run_parser.add_argument(
        "--image",
        required=True,
        help="Pre-built snapshot-ready engine image (see docs/guides/vllm.md)",
    )
    run_parser.add_argument(
        "--image-pull-policy",
        default=None,
        choices=["Always", "IfNotPresent", "Never"],
        help="Overrides the guide's own default (Always). Use IfNotPresent for an "
        "image imported directly into the node's container runtime with no registry.",
    )
    run_parser.add_argument(
        "--model", help="Cached model ID or local directory under /hf-cache"
    )
    run_parser.add_argument(
        "--models", default=str(Path(__file__).resolve().parents[1] / "models.yaml")
    )
    run_parser.add_argument("--mode", choices=["cold_start", "both"], default="both")
    run_parser.add_argument("--output-dir", default=str(results.DEFAULT_RESULTS_ROOT))
    run_parser.add_argument(
        "--keep",
        action="store_true",
        help="Skip cleanup, leave pods/snapshot running for debugging.",
    )
    run_parser.add_argument(
        "--toleration",
        action="append",
        type=_parse_toleration,
        default=[],
        metavar="KEY=VALUE:EFFECT",
        help="Add a toleration to the source/restore pod (repeatable). "
        "e.g. --toleration nvidia.com/gpu=true:NoSchedule for a tainted GPU pool.",
    )
    run_parser.add_argument(
        "--timeout",
        type=int,
        default=run.DEFAULT_TIMEOUT,
        help="Seconds to wait for each stage (source pod Ready, checkpoint Ready, "
        "restore, restore pod Ready) (default: %(default)ss). Raise for models too "
        "big to load within the default window.",
    )
    run_parser.set_defaults(func=cmd_run)

    sweep_parser = subparsers.add_parser(
        "sweep", help="Run every model in a models.yaml sequentially."
    )
    _add_cluster_args(sweep_parser)
    sweep_parser.add_argument("--engine", choices=sorted(ENGINES), default="vllm")
    sweep_parser.add_argument("--image", required=True)
    sweep_parser.add_argument(
        "--image-pull-policy", default=None, choices=["Always", "IfNotPresent", "Never"]
    )
    sweep_parser.add_argument(
        "--models", default=str(Path(__file__).resolve().parents[1] / "models.yaml")
    )
    sweep_parser.add_argument("--mode", choices=["cold_start", "both"], default="both")
    sweep_parser.add_argument("--output-dir", default=str(results.DEFAULT_RESULTS_ROOT))
    sweep_parser.add_argument("--keep", action="store_true")
    sweep_parser.add_argument("--fail-fast", action="store_true")
    sweep_parser.add_argument(
        "--toleration",
        action="append",
        type=_parse_toleration,
        default=[],
        metavar="KEY=VALUE:EFFECT",
        help="Add a toleration to the source/restore pod (repeatable). "
        "e.g. --toleration nvidia.com/gpu=true:NoSchedule for a tainted GPU pool.",
    )
    sweep_parser.add_argument(
        "--timeout",
        type=int,
        default=run.DEFAULT_TIMEOUT,
        help="Seconds to wait for each stage (source pod Ready, checkpoint Ready, "
        "restore, restore pod Ready) (default: %(default)ss). Raise for models too "
        "big to load within the default window.",
    )
    sweep_parser.set_defaults(func=cmd_sweep)

    for command in (run_parser, sweep_parser):
        command.add_argument(
            "--restore-count", type=int, default=3,
            help="Number of restores per checkpoint (default: %(default)s)",
        )
        command.add_argument(
            "--detailed-timing",
            action="store_true",
            help="Patch engine functions and trace imports for cold-start phase timings",
        )
        command.add_argument(
            "--no-drop-page-cache",
            action="store_true",
            help="Skip node page-cache eviction and weight residency verification",
        )
        command.add_argument("--node", required=True, help="Node with enough free GPUs")
        command.add_argument(
            "--model-pvc", required=True, help="Read-only, pre-populated model volume"
        )
        command.add_argument(
            "--work-pvc", required=True, help="Writable cache and log volume"
        )
        command.add_argument("--tp", type=int, help="Tensor-parallel GPU count")
        command.add_argument(
            "--ep", type=int, help="Expert parallelism within the TP GPUs"
        )
        command.add_argument(
            "--engine-args", default="{}", help="Engine constructor arguments as JSON"
        )
        command.add_argument(
            "--dry-run",
            action="store_true",
            help="Write manifests without contacting Kubernetes",
        )

    report_parser = subparsers.add_parser(
        "report",
        help="Render a standalone summary from a results directory (does not edit any doc).",
    )
    report_parser.add_argument("--results-dir", required=True)
    report_parser.add_argument(
        "--out", default=None, help="Write to this file instead of stdout."
    )
    report_parser.set_defaults(func=cmd_report)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
