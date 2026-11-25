# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The credential endpoint the S3 e2e suite runs inside the cluster.

This file is copied into a ConfigMap and runs in a plain Python image, so it
uses only the standard library. A GET with the projected service-account token
in the Authorization header is checked with a TokenReview for the expected
audience, then answered with the store's keys in the container-credentials
format, with an expiry short enough that the clients renew during a run.
"""

import datetime
import json
import os
import ssl
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

AUDIENCE = os.environ["TOKEN_AUDIENCE"]
ACCESS_KEY = os.environ["ACCESS_KEY"]
SECRET_KEY = os.environ["SECRET_KEY"]
LIFETIME_SECONDS = int(os.environ.get("LIFETIME_SECONDS", "900"))
PORT = int(os.environ.get("PORT", "19443"))
SERVICE_ACCOUNT_DIR = "/var/run/secrets/kubernetes.io/serviceaccount"
API = f"https://{os.environ['KUBERNETES_SERVICE_HOST']}:{os.environ['KUBERNETES_SERVICE_PORT']}"
ISSUED = "issued credentials to"


def review(token: str) -> dict:
    body = json.dumps(
        {
            "apiVersion": "authentication.k8s.io/v1",
            "kind": "TokenReview",
            "spec": {"token": token, "audiences": [AUDIENCE]},
        }
    ).encode()
    with open(f"{SERVICE_ACCOUNT_DIR}/token") as handle:
        own_token = handle.read().strip()
    request = urllib.request.Request(
        f"{API}/apis/authentication.k8s.io/v1/tokenreviews",
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {own_token}",
            "Content-Type": "application/json",
        },
    )
    context = ssl.create_default_context(cafile=f"{SERVICE_ACCOUNT_DIR}/ca.crt")
    with urllib.request.urlopen(request, context=context, timeout=10) as response:
        return json.load(response).get("status", {})


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def reply(self, code: int, payload: dict) -> None:
        data = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        token = self.headers.get("Authorization", "")
        if token.lower().startswith("bearer "):
            token = token[7:]
        if not token:
            self.log_message("rejected: no Authorization header")
            return self.reply(401, {"error": "missing token"})
        try:
            status = review(token)
        except Exception as exc:  # noqa: BLE001
            self.log_message("token review failed: %s", exc)
            return self.reply(500, {"error": "token review failed"})
        if not status.get("authenticated"):
            self.log_message("rejected: %s", status.get("error", "not authenticated"))
            return self.reply(403, {"error": "token rejected"})
        if AUDIENCE not in status.get("audiences", []):
            self.log_message("rejected: audience %s", status.get("audiences"))
            return self.reply(403, {"error": "wrong audience"})
        user = status.get("user", {}).get("username", "?")
        expiry = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(
            seconds=LIFETIME_SECONDS
        )
        self.log_message(
            "%s %s until %s client=%s",
            ISSUED,
            user,
            expiry.isoformat(),
            self.headers.get("User-Agent", "?"),
        )
        self.reply(
            200,
            {
                "AccessKeyId": ACCESS_KEY,
                "SecretAccessKey": SECRET_KEY,
                "Token": "",
                "Expiration": expiry.strftime("%Y-%m-%dT%H:%M:%SZ"),
            },
        )


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain("/tls/tls.crt", "/tls/tls.key")
    server.socket = context.wrap_socket(server.socket, server_side=True)
    sys.stderr.write(f"listening on :{PORT} for audience {AUDIENCE}\n")
    server.serve_forever()


if __name__ == "__main__":
    main()
