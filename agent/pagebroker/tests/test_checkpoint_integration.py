# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import unittest

from checkpoint_integration import CheckpointService


class CheckpointFixtureTest(unittest.TestCase):
    def setUp(self):
        self.forwarded = []

        def backend(environment, start_response):
            self.forwarded.append((environment["REQUEST_METHOD"], environment["PATH_INFO"]))
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        self.service = CheckpointService(backend)
        self.service.prefix = "snapshots"

    def request(self, agent, checkpoint, name, method="PUT"):
        environment = {
            "PATH_INFO": f"/bucket/snapshots/{checkpoint}/{name}",
            "QUERY_STRING": "",
            "REQUEST_METHOD": method,
            "HTTP_USER_AGENT": agent,
        }
        if method == "PUT" and name == "index.json":
            environment["HTTP_IF_NONE_MATCH"] = "*"
        statuses = []
        body = self.service(environment, lambda status, headers: statuses.append(status))
        return statuses, b"".join(body)

    def test_tracks_publication_for_sdk_and_crt_clients(self):
        for number, agent in enumerate(("aws-sdk-cpp/1.11.584", "CRTS3NativeClient/0.1.x")):
            with self.subTest(agent=agent):
                checkpoint = f"checkpoint-{number}"
                self.assertEqual(self.request(agent, checkpoint, "data/file")[0], ["200 OK"])
                self.assertEqual(self.request(agent, checkpoint, "index.json")[0], ["200 OK"])
                self.assertEqual(self.service.payload_writes[checkpoint], 1)
                self.assertEqual(self.service.index_writes[checkpoint], 1)
                self.assertEqual(self.service.published_payload_writes[checkpoint], 1)
                self.service.control("assert-no-reupload", checkpoint, lambda status, headers: None)

    def test_injects_lost_responses_for_sdk_and_crt_clients(self):
        for agent in ("aws-sdk-cpp/1.11.584", "CRTS3NativeClient/0.1.x"):
            for checkpoint, name in (("lost-payload", "data/file"), ("lost-index", "index.json")):
                with self.subTest(agent=agent, checkpoint=checkpoint):
                    statuses, body = self.request(agent, checkpoint, name)
                    self.assertEqual(statuses, ["503 Service Unavailable"])
                    self.assertIn(b"ServiceUnavailable", body)
                    self.assertEqual(self.forwarded[-1], ("PUT", f"/bucket/snapshots/{checkpoint}/{name}"))

    def test_unconfirmed_crt_publication_blocks_reads_until_confirmation(self):
        self.assertEqual(self.request("CRTS3NativeClient/0.1.x", "unconfirmed-index", "index.json")[0],
                         ["503 Service Unavailable"])
        self.assertEqual(self.request("aws-sdk-cpp/1.11.584", "unconfirmed-index", "index.json", "HEAD")[0],
                         ["503 Service Unavailable"])
        self.service.control("confirm", "unconfirmed-index", lambda status, headers: None)
        self.assertEqual(self.request("aws-sdk-cpp/1.11.584", "unconfirmed-index", "index.json", "HEAD")[0],
                         ["200 OK"])

    def test_management_client_does_not_trigger_native_faults(self):
        self.assertEqual(self.request("Boto3/1.40.18 Botocore/1.40.76", "lost-index", "index.json")[0],
                         ["200 OK"])
        self.assertEqual(self.service.index_writes["lost-index"], 0)


if __name__ == "__main__":
    unittest.main()
