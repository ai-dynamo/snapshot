# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Build benchmark pods from the corresponding manual guide."""

from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[3]


def set_env(container, name, value):
    for entry in container.setdefault("env", []):
        if entry["name"] == name:
            entry["value"] = value
            return
    container["env"].append(dict(name=name, value=value))


class GuideEngine:
    container_name = "main"

    def _build(
        self,
        filename,
        *,
        name,
        namespace,
        image,
        model,
        image_pull_policy=None,
        tolerations=None,
    ):
        path = ROOT / "docs/guides" / self.guide / filename
        template = yaml.safe_load(path.read_text())["spec"]["template"]
        spec = template["spec"]
        container = next(
            c for c in spec["containers"] if c["name"] == self.container_name
        )
        container["image"] = image
        if image_pull_policy:
            container["imagePullPolicy"] = image_pull_policy
        for key, value in (model.env or {}).items():
            set_env(container, key, value)
        set_env(container, "SNAPSHOT_MODEL", model.hf_id_or_path)
        spec.setdefault("tolerations", []).extend(tolerations or [])
        return dict(
            apiVersion="v1",
            kind="Pod",
            spec=spec,
            metadata=dict(
                template.get("metadata", {}),
                name=name,
                namespace=namespace,
            ),
        )

    def build_source_pod(self, **kwargs):
        return self._build("capture/qwen3-0.6b.yaml", **kwargs)

    def build_restore_pod(self, *, snapshot_name, **kwargs):
        pod = self._build("restore/1-gpu.yaml", **kwargs)
        pod["metadata"].setdefault("annotations", {})["nvidia.com/restore-from"] = (
            snapshot_name
        )
        return pod
