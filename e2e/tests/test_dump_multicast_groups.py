# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free regressions for ``hack/dump-multicast-groups.py``.

The tool attributes the multicast groups in a ``cuinterpose.state`` to their
owning process so ``RestoreMulticastDevices`` failures (ai-dynamo/snapshot#510)
can be triaged. The properties worth pinning are the ones whose failure is
silent: a group dropped from the report hides a device that must be accounted
for at restore, and a malformed file must not crash the triage step. The state
format is internal to cuinterpose, so the tool is best-effort by design; these
tests pin both the happy path (a vLLM-TP2-like manifest) and that tolerance.
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

import msgpack
import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "hack" / "dump-multicast-groups.py"
MiB = 1048576


def _load():
    """Import a module whose filename is not a valid identifier."""
    spec = importlib.util.spec_from_file_location("dump_multicast_groups", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def tool():
    return _load()


def _mc(size_mib, devices, creator, id_):
    return {
        "multicast": {
            "allocation": {"id": id_, "creator_pid": creator},
            "properties": {
                "devices": devices,
                "size": size_mib * MiB,
                "handle_types": 1,
                "flags": 0,
            },
            "virtual_multicast_handle_count": 1,
        }
    }


def _alloc(size_mib, creator, id_):
    return {
        "allocation": {
            "allocation": {"id": id_, "creator_pid": creator},
            "shared": False,
            "peer_access_granted": False,
            "size": size_mib * MiB,
            "allocation_type": 1,
            "handle_types": 1,
            "location": {"location_type": 1, "id": 0},
            "virtual_allocation_handle_count": 1,
        }
    }


def _tp2_manifest():
    # Mirrors a default vLLM TP2 checkpoint: three groups per worker, all
    # created by the rank-0 process (98 MiB CustomAllreduce, 66 MiB symm-mem,
    # 2 MiB NVLS), plus a non-multicast allocation that must be ignored.
    groups = [_mc(98, 2, 100, 1), _mc(66, 2, 100, 2), _mc(2, 2, 100, 3)]
    return {
        100: groups + [_alloc(64, 100, 4)],
        101: groups + [_alloc(64, 101, 5)],
    }


def _write(tmp_path, obj, name="cuinterpose.state"):
    path = tmp_path / name
    path.write_bytes(msgpack.packb(obj, use_bin_type=True))
    return path


@pytest.mark.workload
def test_tp2_three_groups_per_pid(tool, tmp_path):
    path = _write(tmp_path, {"version": 1, "body": _tp2_manifest()})
    per = tool.collect(path)
    assert set(per) == {100, 101}
    for pid, groups in per.items():
        assert len(groups) == 3
        assert sorted(g["size_bytes"] for g in groups) == [2 * MiB, 66 * MiB, 98 * MiB]
        assert all(g["devices"] == 2 for g in groups)
        assert all(g["creator_pid"] == 100 for g in groups)


@pytest.mark.workload
def test_bare_manifest_accepted(tool, tmp_path):
    path = _write(tmp_path, _tp2_manifest())
    assert set(tool.collect(path)) == {100, 101}


@pytest.mark.workload
def test_no_multicast_is_empty(tool, tmp_path):
    path = _write(tmp_path, {"version": 1, "body": {200: [_alloc(8, 200, 9)]}})
    assert tool.collect(path) == {}


@pytest.mark.workload
def test_json_output_roundtrips(tool, tmp_path, capsys):
    path = _write(tmp_path, {"version": 1, "body": _tp2_manifest()})
    assert tool.main([str(path), "--json"]) == 0
    data = json.loads(capsys.readouterr().out)
    assert set(data) == {"100", "101"}
    assert len(data["100"]) == 3


@pytest.mark.workload
def test_null_numeric_fields_tolerated(tool, tmp_path):
    mc = _mc(1, 2, 100, 1)
    mc["multicast"]["properties"]["size"] = None
    mc["multicast"]["properties"]["devices"] = None
    path = _write(tmp_path, {"version": 1, "body": {100: [mc]}})
    per = tool.collect(path)
    assert per[100][0]["size_bytes"] == 0
    assert per[100][0]["devices"] == 0


@pytest.mark.workload
def test_non_dict_allocation_tolerated(tool, tmp_path):
    mc = _mc(1, 2, 100, 1)
    mc["multicast"]["allocation"] = [1, 2]
    path = _write(tmp_path, {"version": 1, "body": {100: [mc]}})
    per = tool.collect(path)
    assert per[100][0]["creator_pid"] is None


@pytest.mark.workload
def test_deep_nesting_does_not_crash(tool):
    # Deeper than Python's recursion limit; the depth guard must stop cleanly.
    node = []
    for _ in range(1500):
        node = [node]
    assert tool.multicast_groups(node) == []


@pytest.mark.workload
def test_unexpected_version_warns(tool, tmp_path, capsys):
    path = _write(tmp_path, {"version": 99, "body": _tp2_manifest()})
    assert tool.main([str(path)]) == 0
    assert "version" in capsys.readouterr().err


@pytest.mark.workload
def test_empty_file_errors(tool, tmp_path, capsys):
    # An empty or truncated state decodes no objects; it must error rather
    # than silently report zero groups (which would hide restore devices).
    path = tmp_path / "empty.state"
    path.write_bytes(b"")
    assert tool.main([str(path)]) == 1
    assert "error" in capsys.readouterr().err


@pytest.mark.workload
def test_undecodable_file_clean_error(tool, tmp_path, capsys):
    path = tmp_path / "bad.state"
    path.write_bytes(b"\xc1\x00\xffnot-msgpack")
    assert tool.main([str(path)]) == 1
    assert "error" in capsys.readouterr().err
