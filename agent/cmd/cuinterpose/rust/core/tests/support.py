# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Shared ctypes declarations for process-isolated fake-driver tests."""

import ctypes as c


class Location(c.Structure):
    _fields_ = [("kind", c.c_int), ("device", c.c_int)]


class Properties(c.Structure):
    _fields_ = [
        ("kind", c.c_int), ("handles", c.c_uint),
        ("location", Location), ("win32", c.c_void_p), ("flags", c.c_byte * 8),
    ]


class Stats(c.Structure):
    _fields_ = [(name, c.c_uint64) for name in (
        "allocations", "handles", "mappings", "multicasts", "exports", "raw", "unsupported",
    )] + [("phase", c.c_uint)]

props = Properties(1, 1, Location(1, 0), None)


def driver():
    """Bind the preloaded fake without initializing the shim's CUDA state."""
    cuda = c.CDLL(None)
    cuda.cuMemCreate.argtypes = [c.POINTER(c.c_uint64), c.c_size_t, c.POINTER(Properties), c.c_uint64]
    cuda.cuMemRelease.argtypes = [c.c_uint64]
    cuda.cuMemExportToShareableHandle.argtypes = [c.c_void_p, c.c_uint64, c.c_uint, c.c_uint64]
    cuda.cuinterpose_debug_stats.argtypes = [c.POINTER(Stats)]
    return cuda


def stats(cuda):
    value = Stats()
    cuda.cuinterpose_debug_stats(c.byref(value))
    return value
