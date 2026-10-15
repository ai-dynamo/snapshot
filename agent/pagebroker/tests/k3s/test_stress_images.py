# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from copy import deepcopy
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from stress_images import canonical_reference, load_identities, package, runtime_identity, validate_identities


def image_document():
    images = {}
    for role, name, config, manifest, revision in (
            ("harness", "stress", "a", "c", "1"), ("broker", "broker", "b", "d", "2")):
        images[role] = {"tag": f"docker.io/local/{name}:test", "reference": f"docker.io/local/{name}@sha256:" + manifest * 64,
                        "manifestDigest": "sha256:" + manifest * 64, "configDigest": "sha256:" + config * 64,
                        "revision": revision * 40, "platform": "linux/amd64"}
    return {"schemaVersion": 1, "images": images}


class ReviewedImagesTest(unittest.TestCase):
    def test_digest_kinds_are_verified_independently(self):
        expected = image_document()["images"]["harness"]
        for value in (expected["configDigest"], "containerd://" + expected["configDigest"]):
            self.assertEqual(runtime_identity(value, expected), "configuration")
        for value in (expected["reference"], "docker-pullable://" + expected["reference"]):
            self.assertEqual(runtime_identity(value, expected), "manifest")
        for value in (expected["manifestDigest"], "sha256:" + "e" * 64,
                      expected["reference"].replace("local/stress", "other/image")):
            with self.subTest(value=value), self.assertRaises(ValueError):
                runtime_identity(value, expected)

    def test_reviewed_document_binds_tag_revision_platform_and_manifest(self):
        original = image_document()
        for name in ("revision", "reference", "platform", "configDigest"):
            value = deepcopy(original)
            value["images"]["broker"][name] = "invalid"
            with self.subTest(name=name), self.assertRaises(ValueError):
                validate_identities(value)
        with self.assertRaises(ValueError):
            validate_identities(original, revisions={"harness": "0" * 40, "broker": "2" * 40})
        with self.assertRaises(ValueError):
            validate_identities(original, tags={"harness": "local/other:test", "broker": "local/broker:test"})
        self.assertEqual(canonical_reference("local/broker:test"), "docker.io/local/broker:test")

    def docker_archive(self, root, corrupt=False):
        path = root / "docker.tar"
        manifests = []
        layer_data = b"fake layer fixture"
        diff_id = "sha256:" + hashlib.sha256(layer_data).hexdigest()
        def add(archive, name, data):
            member = tarfile.TarInfo(name)
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        with tarfile.open(path, "w") as archive:
            add(archive, "layer/layer.tar", layer_data)
            for role, name, revision in (("harness", "stress", "1"), ("broker", "broker", "2")):
                config = {"os": "linux", "architecture": "amd64", "rootfs": {"type": "layers", "diff_ids": [diff_id]},
                          "config": {"Labels": {"org.opencontainers.image.revision": revision * 40}}}
                if corrupt:
                    config["rootfs"]["diff_ids"] = ["sha256:" + "0" * 64]
                add(archive, role + ".json", json.dumps(config).encode())
                manifests.append({"Config": role + ".json", "RepoTags": [f"local/{name}:test"], "Layers": ["layer/layer.tar"]})
            add(archive, "manifest.json", json.dumps(manifests).encode())
        return path

    def test_oci_archive_carries_exact_config_layers_and_digest_aliases(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = package(self.docker_archive(root), root / "oci",
                             {"harness": "local/stress:test", "broker": "local/broker:test"},
                             {"harness": "1" * 40, "broker": "2" * 40})
            with tarfile.open(root / "oci/images.oci.tar") as archive:
                index = json.load(archive.extractfile("index.json"))
                self.assertEqual(len(index["manifests"]), 4)
                for descriptor in index["manifests"]:
                    data = archive.extractfile("blobs/sha256/" + descriptor["digest"][7:]).read()
                    self.assertEqual("sha256:" + hashlib.sha256(data).hexdigest(), descriptor["digest"])
                    manifest = json.loads(data)
                    for blob in [manifest["config"], *manifest["layers"]]:
                        content = archive.extractfile("blobs/sha256/" + blob["digest"][7:]).read()
                        self.assertEqual(len(content), blob["size"])
                        self.assertEqual("sha256:" + hashlib.sha256(content).hexdigest(), blob["digest"])
                self.assertEqual({d["annotations"]["io.containerd.image.name"] for d in index["manifests"]},
                                 {i[key] for i in result["images"].values() for key in ("tag", "reference")})
            self.assertEqual(load_identities(root / "oci/identities.json", {"harness": "1" * 40, "broker": "2" * 40},
                {"harness": "local/stress:test", "broker": "local/broker:test"}), result["images"])

    def test_archive_with_incorrect_layer_hash_or_source_is_rejected(self):
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                with self.assertRaises(ValueError):
                    package(self.docker_archive(root, corrupt), root / "oci",
                            {"harness": "local/stress:test", "broker": "local/broker:test"},
                            {"harness": ("1" if corrupt else "0") * 40, "broker": "2" * 40})
