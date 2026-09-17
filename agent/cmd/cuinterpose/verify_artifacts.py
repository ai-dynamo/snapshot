# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Check the actual shipped ELF artifacts, not just Cargo target names."""

import pathlib
import re
import subprocess
import sys

directory = pathlib.Path(sys.argv[1])
script = pathlib.Path(__file__).with_name("frontend") / "libcuinterpose.ldscript"
global_block = re.search(r"\bglobal:\s*(.*?)\blocal:", script.read_text(), re.DOTALL)
assert global_block, "linker script has no global export block"
frontend = set(re.findall(r"^\s*([A-Za-z_]\w*)\s*;\s*$", global_block.group(1), re.MULTILINE))
assert frontend, "linker script has no frontend exports"
for name, exports in [
    ("libcuinterpose.so", frontend),
    ("libcuinterpose_core.so", {"cuinterpose_core_init"}),
]:
    path = directory / name
    symbols = subprocess.check_output(["nm", "-D", "--defined-only", path], text=True)
    actual = {line.split()[-1] for line in symbols.splitlines()}
    assert actual == exports, (name, actual ^ exports)
    versions = subprocess.check_output(["readelf", "--version-info", "-W", path], text=True)
    assert all(
        tuple(map(int, version.split("."))) <= (2, 34)
        for version in re.findall(r"GLIBC_(\d+(?:\.\d+)+)", versions)
    ), (name, "requires glibc newer than 2.34")
    dynamic = subprocess.check_output(["readelf", "-dW", path], text=True)
    if name == "libcuinterpose_core.so":
        # Runtime installation may run under a DSO constructor's loader lock.
        # No first-use PLT lookup may occur under the installation mutex.
        assert re.search(r"\(FLAGS\).*\bBIND_NOW\b", dynamic), dynamic
    assert not re.search(r"NEEDED.*(?:libcuda|libcudart|libstdc\+\+|libstd-)", dynamic), dynamic
    undefined = subprocess.check_output(["nm", "-D", "--undefined-only", path], text=True)
    assert not re.search(r"\b(?:cu[A-Z]|cuda[A-Z])\w*", undefined), undefined
    assert path.stat().st_mode & 0o777 == 0o644, path

coordinator = directory / "cuinterpose-coordinator"
assert coordinator.stat().st_mode & 0o777 == 0o755
assert "INTERP" not in subprocess.check_output(["readelf", "-lW", coordinator], text=True)
assert "NEEDED" not in subprocess.check_output(["readelf", "-dW", coordinator], text=True)
result = subprocess.run([coordinator], text=True, capture_output=True, check=False)
assert result.returncode == 2 and "Usage: cuinterpose-coordinator" in result.stderr, result
# Clap derives option names; check the shipped CLI rather than Rust source text.
result = subprocess.run([coordinator, "--help"], text=True, capture_output=True, check=True)
flags = {"--prepare", "--restore", "--proc-root", "--checkpoint-dir", "--control-dir", "--process"}
assert flags <= set(result.stdout.split()), result.stdout
print("cuinterpose artifact ABI, permissions, glibc baseline, and static CLI checks passed")
