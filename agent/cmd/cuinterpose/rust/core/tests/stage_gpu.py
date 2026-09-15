#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Stage the locally owned GPU pytest suite and one set of packaged artifacts."""

import argparse
from pathlib import Path
import shutil


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--cuda-checkpoint", type=Path, required=True)
    args = parser.parse_args()
    args.destination.mkdir(parents=True, exist_ok=False)
    source = Path(__file__).resolve().parents[3] / "tests/gpu"
    suite = args.destination / "gpu"
    suite.mkdir()
    for path in source.iterdir():
        if path.is_file() and (path.suffix == ".py" or path.name == "pyproject.toml"):
            shutil.copy2(path, suite / path.name)
    build = args.destination / "build"
    build.mkdir()
    for name in ("libcuinterpose.so", "libcuinterpose_core.so", "cuinterpose-coordinator"):
        shutil.copy2(args.artifacts / name, build / name)
    binary = args.destination / "bin"
    binary.mkdir()
    shutil.copy2(args.cuda_checkpoint, binary / "cuda-checkpoint")
    print(f"Staged local GPU suite in {args.destination}")


if __name__ == "__main__":
    main()
