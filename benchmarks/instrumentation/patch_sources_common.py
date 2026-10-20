# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Instrument selected startup functions in the disposable container filesystem."""

import ast
import hashlib
import importlib.util
import json
import re
from pathlib import Path

PACKAGES = ["vllm", "sglang", "tensorrt_llm", "flashinfer", "tilelang"]
STARTUP_FUNCTIONS = [
    "load_model",
    "load_weights",
    "post_load_weights",
    "process_weights_after_loading",
    "post_load_model_transforms",
    "initialize",
    "init",
    "warmup",
    "prewarm",
    "precompile",
    "compile",
    "autotune",
    "capture",
    "capture_model",
    "capture_graph",
    "init_distributed",
    "initialize_model_parallel",
    "init_parallel",
    "init_device",
    "initialize_kv_cache",
    "profile_run",
    "sampling_module",
]
MODULES = (
    "model_runner",
    "model_loader",
    "model_engine",
    "gpu_worker",
    "parallel_state",
    "cuda_graph_runner",
    "cuda_graph",
    "deepseek_v4",
    "sampling",
    "autotuner",
    "jit",
    "quantization",
    "weight",
    "loader",
    "sampler",
    "compil",
)
INIT_MODULES = (
    "model_runner",
    "model_engine",
    "gpu_worker",
    "parallel_state",
    "cuda_graph_runner",
)

PROFILES = {
    "base": ([], [], []),
    "expanded": (["cutlass"], ["build_and_load", "run_ninja"], ["base_dsl/dsl.py"]),
    "deepgemm": (
        ["cutlass"],
        ["build_and_load", "run_ninja"],
        ["base_dsl/dsl.py", "model_executor/warmup/deep_gemm_warmup.py"],
    ),
}


def is_generator(node):
    """Yield in a nested function does not make its enclosing function a generator."""
    pending = list(node.body)
    while pending:
        child = pending.pop()
        if isinstance(child, (ast.Yield, ast.YieldFrom)):
            return True
        if isinstance(
            child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda)
        ):
            continue
        pending.extend(ast.iter_child_nodes(child))
    return False


def instrument(profile):
    extra_packages, extra_functions, extra_modules = PROFILES[profile]
    packages = PACKAGES + extra_packages
    modules = MODULES + tuple(extra_modules)
    startup = re.compile(
        r"(^|_)(" + "|".join(extra_functions + STARTUP_FUNCTIONS) + r")($|_)"
    )
    changes = []
    for package in packages:
        spec = importlib.util.find_spec(package)
        if spec is None:
            continue
        for root in spec.submodule_search_locations or []:
            for path in Path(root).rglob("*.py"):
                relative = str(path.relative_to(root))
                if not any(s in relative for s in modules):
                    continue
                source = path.read_text()
                if "from startup_timing import phase as _startup_phase" in source:
                    continue
                tree = ast.parse(source)
                lines = source.splitlines(True)
                edits = []
                names = []

                def visit(body, prefix=""):
                    for node in body:
                        if isinstance(node, ast.ClassDef):
                            visit(node.body, prefix + node.name + ".")
                        elif isinstance(node, ast.FunctionDef):
                            name = prefix + node.name
                            select = startup.search(node.name) or (
                                node.name == "__init__"
                                and any(s in relative for s in INIT_MODULES)
                            )
                            if not select:
                                continue
                            # Generator execution starts at iteration, not function call.
                            if is_generator(node):
                                continue
                            label = (
                                package
                                + "."
                                + relative[:-3].replace("/", ".")
                                + "."
                                + name
                            )
                            edits.append(
                                (
                                    node.lineno - 1,
                                    " " * node.col_offset
                                    + f"@_startup_phase({label!r})\n",
                                )
                            )
                            names.append(label)

                visit(tree.body)
                if not edits:
                    continue
                insert = 0
                for node in tree.body:
                    if (
                        isinstance(node, ast.Expr)
                        and isinstance(node.value, ast.Constant)
                        and isinstance(node.value.value, str)
                        and insert == 0
                    ):
                        insert = node.end_lineno
                    elif (
                        isinstance(node, ast.ImportFrom) and node.module == "__future__"
                    ):
                        insert = node.end_lineno
                    else:
                        break
                edits.append(
                    (insert, "from startup_timing import phase as _startup_phase\n")
                )
                for index, text in sorted(edits, reverse=True):
                    lines.insert(index, text)
                patched = "".join(lines)
                compile(patched, str(path), "exec")
                path.write_text(patched)
                changes.append(
                    dict(
                        file=str(path),
                        sha256_before=hashlib.sha256(source.encode()).hexdigest(),
                        sha256_after=hashlib.sha256(patched.encode()).hexdigest(),
                        functions=names,
                    )
                )
    assert changes, "No instrumentation applied"
    print("STARTUP_INSTRUMENTATION " + json.dumps(changes), flush=True)
