# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CPU/S3 destination lifetime regression against the real native SDK.

Each scenario runs in a separate, time-limited process: mprotect turns late
native access into a test failure. Uses a local HTTP endpoint and fake
credentials; no cloud account or Python packages are needed.
"""

import argparse
import ctypes as C
import http.server
import mmap
import os
from pathlib import Path
import resource
import subprocess
import sys
import threading
import time
import unittest


# Values from the pinned SDK's streamer/response_code.h and streamer/device.h.
SUCCESS = 0
FILE_ACCESS_ERROR = 2
TIMED_OUT = 16
CPU = 0


class Device(C.Structure):
    _fields_ = [("type", C.c_int), ("id", C.c_int)]


class SubmissionLifetimeTest(unittest.TestCase):
    def exercise(self, retry=False, fail_b=False):
        release_b = threading.Event()
        b_started = threading.Event()
        retries = []
        sizes = {"a0": 7 * 1024 * 1024, "a1": 1024 * 1024, "b": 7 * 1024 * 1024}

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_HEAD(self):
                key = self.path.rsplit("/", 1)[-1]
                self.send_response(200)
                self.send_header("Content-Length", str(sizes[key]))
                self.send_header("ETag", '"test-object"')
                self.end_headers()

            def do_GET(self):
                key = self.path.rsplit("/", 1)[-1]
                if key == "b":
                    b_started.set()
                    if not release_b.wait(20):
                        self.send_error(500)
                        return
                if key == "b" and fail_b:
                    self.send_error(404)
                    return
                if key == "a0" and retry and not retries:
                    retries.append(True)
                    self.send_error(503)
                    return
                start, end = 0, sizes[key] - 1
                if "Range" in self.headers:
                    start, end = map(int, self.headers["Range"].removeprefix("bytes=").split("-"))
                self.send_response(206)
                self.send_header("Content-Length", str(end - start + 1))
                self.send_header("Content-Range", f"bytes {start}-{end}/{sizes[key]}")
                self.send_header("ETag", '"test-object"')
                self.end_headers()
                self.wfile.write(b"x" * (end - start + 1))

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server_thread = threading.Thread(target=server.serve_forever, daemon=True)
        server_thread.start()
        handle = C.c_void_p()
        mappings = []
        try:
            lib = C.CDLL("libstreamer.so")
            lib.runai_file_streamer_start.restype = C.c_int
            lib.runai_file_streamer_start.argtypes = [C.POINTER(C.c_void_p)]
            lib.runai_file_streamer_end.argtypes = [C.c_void_p]
            lib.runai_file_streamer_end.restype = None
            lib.runai_file_streamer_set_credentials.argtypes = [
                C.c_void_p, C.POINTER(C.c_char_p), C.POINTER(C.c_char_p), C.c_uint]
            lib.runai_file_streamer_request.argtypes = [
                C.c_void_p, C.POINTER(C.c_uint64), C.c_uint, C.POINTER(C.c_char_p),
                C.POINTER(C.c_uint), C.POINTER(C.c_size_t), C.POINTER(C.c_size_t),
                C.POINTER(C.c_void_p), Device]
            lib.runai_file_streamer_response.argtypes = [
                C.c_void_p, C.POINTER(C.c_uint64), C.POINTER(C.c_uint),
                C.POINTER(C.c_uint), C.POINTER(C.c_int), C.c_uint]
            self.assertEqual(lib.runai_file_streamer_start(C.byref(handle)), SUCCESS)
            keys = (C.c_char_p * 4)(b"region", b"endpoint", b"access_key_id", b"secret_access_key")
            values = (C.c_char_p * 4)(b"us-east-1", f"http://127.0.0.1:{server.server_port}".encode(),
                                     b"fake-access", b"fake-secret")
            self.assertEqual(lib.runai_file_streamer_set_credentials(handle, keys, values, 4), SUCCESS)

            def submit(names):
                buffers = [mmap.mmap(-1, sizes[name]) for name in names]
                mappings.extend(buffers)
                addresses = [C.addressof(C.c_char.from_buffer(buffer)) for buffer in buffers]
                count = len(names)
                paths = (C.c_char_p * count)(*[f"s3://test-bucket/{name}".encode() for name in names])
                ranges = (C.c_uint * count)(*[1] * count)
                offsets = (C.c_size_t * count)(*[0] * count)
                lengths = (C.c_size_t * count)(*[sizes[name] for name in names])
                destinations = (C.c_void_p * count)(*addresses)
                submission = C.c_uint64()
                self.assertEqual(lib.runai_file_streamer_request(
                    handle, C.byref(submission), count, paths, ranges, offsets, lengths,
                    destinations, Device(CPU, 0)), SUCCESS)
                self.assertNotEqual(submission.value, 0)
                return submission.value, buffers, addresses

            a_id, a_buffers, a_addresses = submit(["a0", "a1"])
            b_id, _, _ = submit(["b"])
            self.assertNotEqual(a_id, b_id)
            self.assertTrue(b_started.wait(10), "B never began reading")
            a_files = set()
            deadline = time.monotonic() + 20
            while len(a_files) != 2:
                self.assertLess(time.monotonic(), deadline, "A failed to finish while B was blocked")
                sid, file, range_id, done = C.c_uint64(), C.c_uint(), C.c_uint(), C.c_int()
                status = lib.runai_file_streamer_response(
                    handle, C.byref(sid), C.byref(file), C.byref(range_id), C.byref(done), 100)
                if status == TIMED_OUT:
                    continue
                self.assertEqual(status, SUCCESS)
                self.assertEqual(sid.value, a_id)
                self.assertEqual(range_id.value, 0)
                self.assertIn(file.value, (0, 1))
                self.assertNotIn(file.value, a_files)
                a_files.add(file.value)
                self.assertEqual(done.value, int(len(a_files) == 2))
            if retry:
                self.assertTrue(retries, "retry path was not exercised")

            libc = C.CDLL(None, use_errno=True)
            libc.mprotect.restype = C.c_int
            libc.mprotect.argtypes = [C.c_void_p, C.c_size_t, C.c_int]
            for buffer, address in zip(a_buffers, a_addresses):
                self.assertEqual(buffer[:], b"x" * len(buffer))
                # Keep the address reserved but inaccessible through B's reads and teardown.
                # Unlike simply freeing it, this cannot hide late writes through address reuse.
                self.assertEqual(libc.mprotect(address, len(buffer), 0), 0)
            release_b.set()
            deadline = time.monotonic() + 20
            while True:
                self.assertLess(time.monotonic(), deadline, "B failed to finish")
                status = lib.runai_file_streamer_response(
                    handle, C.byref(sid), C.byref(file), C.byref(range_id), C.byref(done), 100)
                if status == TIMED_OUT:
                    continue
                self.assertEqual(sid.value, b_id)
                self.assertEqual(status, FILE_ACCESS_ERROR if fail_b else SUCCESS)
                self.assertEqual(file.value, 0)
                self.assertEqual(range_id.value, 0)
                self.assertEqual(done.value, 1)
                break
        finally:
            release_b.set()
            if handle.value:
                lib.runai_file_streamer_end(handle)
            for buffer in mappings:
                buffer.close()
            server.shutdown()
            server.server_close()
            server_thread.join()

    def test_successful_submission_releases_buffers_before_other_submission(self):
        self.exercise()

    def test_success_after_retry_releases_buffers(self):
        self.exercise(retry=True)

    def test_later_submission_failure_does_not_touch_completed_buffers(self):
        self.exercise(fail_b=True)


CASES = (
    "test_successful_submission_releases_buffers_before_other_submission",
    "test_success_after_retry_releases_buffers",
    "test_later_submission_failure_does_not_touch_completed_buffers",
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=CASES, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.case:
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        unittest.main(argv=[sys.argv[0], "SubmissionLifetimeTest." + args.case, "-v"])
        return

    # Each child gets fresh native AWS state and no inherited cloud credentials.
    environment = {name: value for name, value in os.environ.items()
                   if not name.startswith(("AWS_", "RUNAI_STREAMER_"))}
    environment.update(
        AWS_CONFIG_FILE=os.devnull,
        AWS_SHARED_CREDENTIALS_FILE=os.devnull,
        AWS_EC2_METADATA_DISABLED="true",
        RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING="0",
        RUNAI_STREAMER_CONCURRENCY="4",
        RUNAI_STREAMER_CHUNK_BYTESIZE=str(5 * 1024 * 1024),
        RUNAI_STREAMER_S3_TIMEOUT="10",
        RUNAI_STREAMER_S3_MAX_RETRIES="2",
        RUNAI_STREAMER_LOG_LEVEL="ERROR",
        NO_PROXY="127.0.0.1,localhost",
        no_proxy="127.0.0.1,localhost",
    )
    failures = 0
    for case in CASES:
        try:
            subprocess.run([sys.executable, str(Path(__file__).resolve()), "--case", case],
                           env=environment, check=True, timeout=60)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
            print(f"Native lifetime regression failed: {case}: {error}", file=sys.stderr)
            failures += 1
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
