# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json
from types import SimpleNamespace
import unittest

import render


def options(mode):
    return SimpleNamespace(mode=mode, namespace="pagebroker-smoke-test", client_image="local/client:test",
                           pagebroker_image="local/pagebroker:test", storage_class="local-path",
                           image_pull_policy="Never", node="test-node", bucket="smoke-bucket", region="us-east-2",
                           endpoint=None, prefix="test/smoke", credentials_secret="test-credentials",
                           sample_interval=1, settle_seconds=5)


class ManifestTest(unittest.TestCase):
    def test_filesystem_lifecycle_and_mounts(self):
        resources, job, exporter = render.render(options("filesystem"))
        spec = job["spec"]["template"]["spec"]
        broker, client = spec["containers"]
        self.assertEqual(job["spec"]["backoffLimit"], 0)
        self.assertEqual(spec["restartPolicy"], "Never")
        self.assertTrue(spec["shareProcessNamespace"])
        self.assertFalse(spec["automountServiceAccountToken"])
        self.assertEqual(spec["securityContext"]["runAsUser"], spec["securityContext"]["fsGroup"])
        self.assertNotIn("ttlSecondsAfterFinished", job["spec"])
        self.assertNotIn("--storage-config", broker["args"])
        for container in (broker, client):
            mounts = {item["name"]: item["mountPath"] for item in container["volumeMounts"]}
            self.assertEqual(mounts["socket"], "/shared")
            self.assertEqual(mounts["staging"], "/staging")
            self.assertEqual(mounts["storage"], "/storage")
        volumes = {item["name"]: item for item in spec["volumes"]}
        self.assertEqual(volumes["staging"]["emptyDir"], {"medium": "Memory", "sizeLimit": "1Gi"})
        claim = next(item for item in resources if item["kind"] == "PersistentVolumeClaim")
        self.assertEqual(volumes["results"]["persistentVolumeClaim"]["claimName"], claim["metadata"]["name"])
        self.assertEqual(exporter["spec"]["volumes"][0]["persistentVolumeClaim"]["claimName"], claim["metadata"]["name"])
        self.assertTrue(exporter["spec"]["containers"][0]["volumeMounts"][0]["readOnly"])
        self.assertEqual(spec["nodeSelector"], exporter["spec"]["nodeSelector"])

    def test_s3_configuration_and_secret_are_bound_to_the_broker(self):
        args = options("s3")
        resources, job, _ = render.render(args)
        pod = job["spec"]["template"]["spec"]
        broker, client = pod["containers"]
        config = next(item for item in resources if item["kind"] == "ConfigMap")
        storage = json.loads(config["data"]["storage.json"])
        self.assertEqual(storage["prefix"], "test/smoke/" + args.namespace)
        self.assertEqual(storage["endpoint"], "https://s3.us-east-2.amazonaws.com")
        self.assertGreaterEqual(storage["limits"]["uploadPartBytes"], 8 * 1024**2)
        self.assertGreaterEqual(storage["limits"]["uploadBufferBytes"], 1024**3)
        self.assertIn("--storage-config", broker["args"])
        self.assertEqual(dict((item["name"], item["value"]) for item in client["env"])["PAGEBROKER_SMOKE_STORE_ID"], storage["storeId"])
        self.assertNotIn("credentials", [item["name"] for item in client["volumeMounts"]])
        environment = {item["name"]: item["valueFrom"]["secretKeyRef"] for item in broker["env"]}
        self.assertEqual(set(environment), {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"})
        for name, ref in environment.items():
            self.assertEqual(ref["name"], "test-credentials")
            self.assertEqual(ref["key"], name)
            self.assertEqual(ref["optional"], name == "AWS_SESSION_TOKEN")
        self.assertNotIn("uploadTargetGbps", storage["limits"])
        self.assertFalse(any(item["kind"] == "Secret" for item in resources))

    def test_invalid_namespace_endpoint_and_prefix_are_rejected(self):
        for field, value in (("namespace", "snapshot"), ("namespace", "pagebroker-smoke-../bad"),
                             ("endpoint", "http://s3.example.com"), ("endpoint", "https://user:secret@example.com"),
                             ("prefix", "../outside"), ("prefix", "/absolute"), ("prefix", "space here"),
                             ("sample_interval", 0), ("sample_interval", float("nan")), ("settle_seconds", -1),
                             ("bucket", None), ("region", None)):
            with self.subTest(field=field, value=value):
                args = options("s3")
                setattr(args, field, value)
                with self.assertRaises(ValueError):
                    render.render(args)

    def test_monitoring_configuration_reaches_only_the_client(self):
        args = options("filesystem")
        args.sample_interval, args.settle_seconds = 0.5, 10
        _, job, _ = render.render(args)
        broker, client = job["spec"]["template"]["spec"]["containers"]
        self.assertEqual(client["args"], ["--sample-interval", "0.5", "--settle-seconds", "10"])
        self.assertNotIn("--sample-interval", broker["args"])


if __name__ == "__main__":
    unittest.main()
