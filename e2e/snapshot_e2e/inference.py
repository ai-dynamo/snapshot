# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inference requests against a framework guide program, from inside its pod.

The guide programs expose `POST /generate` with `{"prompt": ...}` on port 8000
and answer `{"text": ...}`. There is no Service in front of them, so the
request is issued from inside the pod through exec. Python's standard library
is used rather than curl because every framework runtime image ships Python
and none is guaranteed to ship curl.
"""

from __future__ import annotations

import json
import shlex

from snapshot_e2e import k8s
from snapshot_e2e.frameworks import API_PORT
from snapshot_e2e.frameworks import REQUEST_TIMEOUT_SECONDS

_CLIENT = """
import json, sys, urllib.request
payload = json.dumps({"prompt": sys.argv[1]}).encode()
request = urllib.request.Request(
    sys.argv[2],
    data=payload,
    headers={"Content-Type": "application/json"},
    method="POST",
)
with urllib.request.urlopen(request, timeout=float(sys.argv[3])) as response:
    body = response.read().decode()
print("__snapshot_e2e_generate__" + body)
"""

_MARKER = "__snapshot_e2e_generate__"


def request_generate(
    namespace: str,
    pod: str,
    prompt: str,
    *,
    timeout: int = REQUEST_TIMEOUT_SECONDS,
    port: int = API_PORT,
    container: str | None = None,
) -> str:
    """POSTs the prompt to the program's /generate and returns the generated text.

    Raises AssertionError if the endpoint does not answer with non-empty text;
    the raw exec output is included so a framework error is visible verbatim.
    """
    url = f"http://127.0.0.1:{port}/generate"
    command = (
        f"python3 -c {shlex.quote(_CLIENT)} {shlex.quote(prompt)} "
        f"{shlex.quote(url)} {timeout}"
    )
    output = k8s.exec_command(namespace, pod, command, container=container)
    marker_at = output.rfind(_MARKER)
    if marker_at < 0:
        raise AssertionError(
            f"/generate on {namespace}/{pod} produced no response; exec output:\n{output}"
        )
    body = output[marker_at + len(_MARKER):].strip()
    try:
        text = json.loads(body)["text"]
    except (ValueError, KeyError, TypeError) as exc:
        raise AssertionError(
            f"/generate on {namespace}/{pod} returned malformed body {body!r}: {exc}"
        ) from exc
    if not isinstance(text, str) or not text.strip():
        raise AssertionError(f"/generate on {namespace}/{pod} returned empty text: {body!r}")
    return text


_FILE_MARKER = "__snapshot_e2e_file__"


def read_control_file(
    namespace: str, pod: str, path: str, *, container: str | None = None
) -> str | None:
    """Content of a control-directory file, or None when it does not exist.

    exec merges stdout and stderr and hides the remote exit status, so a bare
    `cat` of a missing file would come back as its error text and pass a
    "non-empty" assertion. The marker is printed only when the file exists.
    """
    quoted = shlex.quote(path)
    output = k8s.exec_command(
        namespace,
        pod,
        f"if [[ -f {quoted} ]]; then printf '%s' {shlex.quote(_FILE_MARKER)}; cat {quoted}; fi",
        container=container,
    )
    return parse_control_file(output)


def parse_control_file(output: str) -> str | None:
    if not output.startswith(_FILE_MARKER):
        return None
    return output[len(_FILE_MARKER):]
