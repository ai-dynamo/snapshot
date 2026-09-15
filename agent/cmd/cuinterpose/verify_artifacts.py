# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Check the actual shipped ELF artifacts, not just Cargo target names."""

import pathlib
import re
import subprocess
import sys

directory = pathlib.Path(sys.argv[1])
frontend = {
    "dlsym", "cuInit", "cuGetProcAddress", "cuGetProcAddress_v2",
    "cuGetProcAddress_v2_ptsz", "cudaGetDriverEntryPoint",
    "cudaGetDriverEntryPoint_ptsz", "cudaGetDriverEntryPointByVersion",
    "cudaGetDriverEntryPointByVersion_ptsz", "cuMemCreate", "cuMemRelease",
    "cuMemRetainAllocationHandle", "cuMemMap", "cuMemUnmap", "cuMemSetAccess",
    "cuMemExportToShareableHandle", "cuMemImportFromShareableHandle",
    "cuMemGetAllocationPropertiesFromHandle", "cuMulticastCreate",
    "cuMulticastAddDevice", "cuMulticastBindMem", "cuMulticastBindMem_v2",
    "cuMulticastBindAddr", "cuMulticastBindAddr_v2", "cuMulticastGetGranularity",
    "cuMulticastUnbind", "cuinterpose_build_info", "cuinterpose_debug_stats",
}
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
    assert not re.search(r"NEEDED.*(?:libcuda|libcudart|libstdc\+\+|libstd-)", dynamic), dynamic
    assert path.stat().st_mode & 0o777 == 0o644, path

coordinator = directory / "cuinterpose-coordinator"
assert coordinator.stat().st_mode & 0o777 == 0o755
assert "INTERP" not in subprocess.check_output(["readelf", "-lW", coordinator], text=True)
assert "NEEDED" not in subprocess.check_output(["readelf", "-dW", coordinator], text=True)
result = subprocess.run([coordinator], text=True, capture_output=True, check=False)
assert result.returncode == 2 and "Usage: cuinterpose-coordinator" in result.stderr, result
print("cuinterpose artifact ABI, permissions, glibc baseline, and static CLI checks passed")
