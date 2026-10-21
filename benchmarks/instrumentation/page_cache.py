# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Clear the node page cache and verify model weights before engine startup."""

import argparse
import ctypes
import json
import os
from pathlib import Path

DROP_CACHES = Path("/proc/sys/vm/drop_caches")


def resident_pages(fd, size):
    page_size = os.sysconf("SC_PAGE_SIZE")
    pages = (size + page_size - 1) // page_size
    libc = ctypes.CDLL(None, use_errno=True)
    libc.mmap.restype = ctypes.c_void_p
    libc.mmap.argtypes = [
        ctypes.c_void_p,
        ctypes.c_size_t,
        ctypes.c_int,
        ctypes.c_int,
        ctypes.c_int,
        ctypes.c_longlong,
    ]
    libc.mincore.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p]
    libc.munmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
    # A PROT_NONE, MAP_PRIVATE mapping does not read the weights.
    address = libc.mmap(None, size, 0, 2, fd, 0)
    if address == ctypes.c_void_p(-1).value:
        raise OSError(ctypes.get_errno(), "Cannot map weight file for mincore")
    try:
        vector = (ctypes.c_ubyte * pages)()
        if libc.mincore(address, size, vector) != 0:
            raise OSError(ctypes.get_errno(), "Cannot query weight-file residency")
        return sum(value & 1 for value in vector), pages
    finally:
        if libc.munmap(address, size) != 0:
            raise OSError(ctypes.get_errno(), "Cannot unmap weight-file probe")


def prepare(
    model,
    revision="main",
    output=Path("/startup-spans/cache-control.json"),
    drop_page_cache=True,
):
    report = dict(success=False, node_page_cache_dropped=False, files=[])
    try:
        root = Path(model)
        if not root.is_dir():
            from huggingface_hub import snapshot_download

            root = Path(
                snapshot_download(
                    repo_id=model,
                    revision=revision,
                    local_files_only=True,
                )
            )
        weights = sorted(root.glob("*.safetensors"))
        if not weights:
            weights = sorted(root.glob("pytorch_model*.bin"))
        if not weights or any(p.stat().st_size == 0 for p in weights):
            raise ValueError(
                "No nonempty safetensors or PyTorch weight files: " + str(root)
            )
        report["model_directory"] = str(root)
        report["files"] = [dict(file=p.name, bytes=p.stat().st_size) for p in weights]
        report["weights_bytes"] = sum(entry["bytes"] for entry in report["files"])
        if not drop_page_cache:
            report["eviction_skipped"] = True
            report["success"] = True
            return report
        # Open first: permission failures must stop before sync or engine startup.
        try:
            control = DROP_CACHES.open("w")
        except OSError as error:
            raise PermissionError(
                "Page-cache drop requires a privileged init container and writable "
                "/proc/sys/vm/drop_caches. Use --no-drop-page-cache to opt out."
            ) from error
        with control:
            os.sync()
            control.write("1\n")
            control.flush()
        report["node_page_cache_dropped"] = True
        for path, entry in zip(weights, report["files"]):
            with path.open("rb") as stream:
                fd = stream.fileno()
                os.fsync(fd)
                before, total = resident_pages(fd, path.stat().st_size)
                os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
                after, _ = resident_pages(fd, path.stat().st_size)
            entry.update(
                dict(
                    total_pages=total,
                    resident_pages_before=before,
                    resident_pages_after=after,
                )
            )
        report["resident_pages_after"] = sum(
            entry["resident_pages_after"] for entry in report["files"]
        )
        report["total_pages"] = sum(entry["total_pages"] for entry in report["files"])
        if report["resident_pages_after"]:
            raise RuntimeError(
                "Model weights remain resident after page-cache eviction; "
                "check concurrent readers and permission to query mincore."
            )
        report["success"] = True
        return report
    except Exception as error:
        report["error"] = str(error)
        raise
    finally:
        output.write_text(json.dumps(report, indent=2) + "\n")
        print("BENCHMARK_PAGE_CACHE " + json.dumps(report), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("model")
    parser.add_argument("revision", nargs="?", default="main")
    parser.add_argument(
        "--no-drop-page-cache", action="store_false", dest="drop_page_cache"
    )
    prepare(**vars(parser.parse_args()))
