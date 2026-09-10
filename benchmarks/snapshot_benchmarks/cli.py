# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CLI entry point: `python -m snapshot_benchmarks <command>`.

See docs/development/benchmarks-guide.md for the full walkthrough.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

import yaml

from snapshot_e2e import k8s

from snapshot_benchmarks import metadata, report, results
from snapshot_benchmarks.engines import ModelSpec
from snapshot_benchmarks.engines.vllm import VLLMEngine
from snapshot_benchmarks import run
from snapshot_benchmarks.run import BenchmarkConfig, run_benchmark

ENGINES = {"vllm": VLLMEngine()}


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
    parser.add_argument("--release", default=None, help="Helm release name. Defaults to E2EConfig.from_env().")
    parser.add_argument("--pvc-name", default=None, help="Checkpoint PVC name. Defaults to E2EConfig.from_env().")


def _benchmark_config(args: argparse.Namespace) -> BenchmarkConfig:
    base = k8s.E2EConfig.from_env()
    namespace = args.namespace or base.namespace
    return BenchmarkConfig(
        workload_namespace=namespace,
        snapshot_namespace=args.snapshot_namespace or namespace,
        release=args.release or base.release,
        pvc_name=args.pvc_name or base.pvc_name,
        kubeconfig=base.kubeconfig,
    )


def _stringify_env(label: str, env: dict[str, Any] | None) -> dict[str, str]:
    """Coerces scalar `env` values (e.g. an unquoted `0.75` or `2048` in YAML,
    parsed by PyYAML as a float/int, not a string) to `str`, since pod env var
    values are always strings anyway. Raises a clear, load-time error for
    anything that isn't a plain scalar (list/dict/None) instead of letting a
    malformed value reach `create_pod` and fail deep inside a Kubernetes API
    JSON-unmarshal error."""
    result: dict[str, str] = {}
    for key, value in (env or {}).items():
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
            reported_weights_bytes=entry.get("reported_weights_bytes"),
            env=_stringify_env(entry["label"], entry.get("env")),
        )
        for entry in data["models"]
    ]


def cmd_metadata(args: argparse.Namespace) -> int:
    cfg = _benchmark_config(args)
    k8s.configure(cfg.workload_e2e_config())
    env = metadata.collect_environment(namespace=cfg.workload_namespace, pvc_name=cfg.pvc_name)
    print(json.dumps(_to_json(env), indent=2))
    return 0


def _to_json(dataclass_instance) -> dict:
    import dataclasses

    return dataclasses.asdict(dataclass_instance)


def cmd_run(args: argparse.Namespace) -> int:
    cfg = _benchmark_config(args)
    k8s.configure(cfg.workload_e2e_config())
    engine = ENGINES[args.engine]
    models = {m.label: m for m in _load_models(Path(args.models))}
    if args.model_label not in models:
        print(f"error: {args.model_label!r} not found in {args.models}", file=sys.stderr)
        print(f"available: {sorted(models)}", file=sys.stderr)
        return 2
    model = models[args.model_label]

    result = run_benchmark(
        cfg,
        engine,
        model,
        image=args.image,
        image_pull_policy=args.image_pull_policy,
        tolerations=args.toleration,
        mode=args.mode,
        keep=args.keep,
        pod_ready_timeout=args.pod_ready_timeout,
        snapshot_ready_timeout=args.pod_ready_timeout,
        restore_timeout=args.pod_ready_timeout,
    )
    out_dir = results.invocation_dir(Path(args.output_dir), git_sha=result.git_sha)
    path = results.write_result(result, out_dir)
    print(f"wrote {path}")
    return 0


def cmd_sweep(args: argparse.Namespace) -> int:
    cfg = _benchmark_config(args)
    k8s.configure(cfg.workload_e2e_config())
    engine = ENGINES[args.engine]
    models = _load_models(Path(args.models))

    out_dir: Path | None = None
    failures: list[str] = []
    for model in models:
        print(f"=== {model.label} ===")
        try:
            result = run_benchmark(
                cfg,
                engine,
                model,
                image=args.image,
                image_pull_policy=args.image_pull_policy,
                tolerations=args.toleration,
                mode=args.mode,
                keep=args.keep,
                pod_ready_timeout=args.pod_ready_timeout,
                snapshot_ready_timeout=args.pod_ready_timeout,
                restore_timeout=args.pod_ready_timeout,
            )
        except Exception as exc:  # noqa: BLE001 - recorded per-model, sweep continues
            print(f"error: {model.label} failed: {exc}", file=sys.stderr)
            failures.append(model.label)
            if args.fail_fast:
                raise
            continue
        if out_dir is None:
            out_dir = results.invocation_dir(Path(args.output_dir), git_sha=result.git_sha)
        path = results.write_result(result, out_dir)
        print(f"wrote {path}")

    if failures:
        print(f"sweep finished with {len(failures)} failure(s): {failures}", file=sys.stderr)
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
    run_parser.add_argument("--model-label", required=True, help='e.g. "Qwen3 0.6B", must match models.yaml')
    run_parser.add_argument("--image", required=True, help="Pre-built snapshot-ready engine image (see docs/guides/vllm.md)")
    run_parser.add_argument(
        "--image-pull-policy",
        default=None,
        choices=["Always", "IfNotPresent", "Never"],
        help="Overrides the guide's own default (Always). Use IfNotPresent for an "
        "image imported directly into the node's container runtime with no registry.",
    )
    run_parser.add_argument("--models", default=str(Path(__file__).resolve().parents[1] / "models.yaml"))
    run_parser.add_argument("--mode", choices=["cold_start", "both"], default="both")
    run_parser.add_argument("--output-dir", default=str(results.DEFAULT_RESULTS_ROOT))
    run_parser.add_argument("--keep", action="store_true", help="Skip cleanup, leave pods/snapshot running for debugging.")
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
        "--pod-ready-timeout",
        type=int,
        default=run.DEFAULT_POD_READY_TIMEOUT,
        help="Seconds to wait for the source/restore pod to become Ready and for the "
        "checkpoint to become Ready (default: %(default)ss). Raise for models too big "
        "to load within the default window.",
    )
    run_parser.set_defaults(func=cmd_run)

    sweep_parser = subparsers.add_parser("sweep", help="Run every model in a models.yaml sequentially.")
    _add_cluster_args(sweep_parser)
    sweep_parser.add_argument("--engine", choices=sorted(ENGINES), default="vllm")
    sweep_parser.add_argument("--image", required=True)
    sweep_parser.add_argument(
        "--image-pull-policy", default=None, choices=["Always", "IfNotPresent", "Never"]
    )
    sweep_parser.add_argument("--models", default=str(Path(__file__).resolve().parents[1] / "models.yaml"))
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
        "--pod-ready-timeout",
        type=int,
        default=run.DEFAULT_POD_READY_TIMEOUT,
        help="Seconds to wait for the source/restore pod to become Ready and for the "
        "checkpoint to become Ready (default: %(default)ss). Raise for models too big "
        "to load within the default window.",
    )
    sweep_parser.set_defaults(func=cmd_sweep)

    report_parser = subparsers.add_parser(
        "report", help="Render a standalone summary from a results directory (does not edit any doc)."
    )
    report_parser.add_argument("--results-dir", required=True)
    report_parser.add_argument("--out", default=None, help="Write to this file instead of stdout.")
    report_parser.set_defaults(func=cmd_report)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
