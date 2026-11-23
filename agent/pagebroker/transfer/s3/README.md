<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3 client

`S3Client` runs [rclone v1.75.2](https://github.com/rclone/rclone/releases/tag/v1.75.2).
The PageBroker Dockerfile downloads the official prebuilt Linux executable for
amd64 or arm64 and verifies its release archive against a pinned SHA-256 checksum. It does not
compile or modify rclone. Matching sources and modules are downloaded only for
attribution. License information, dependency notices, archive checksums and binary
build information are packaged under `/legal/rclone`. PageBroker no longer builds
or links the AWS C++ SDK.
Model Streamer's prebuilt S3 plugin retains its own AWS implementation.

The client exposes `UploadFiles`, `Head`, bounded `Get`, and create-only `Put`.
`UploadFiles` runs one `copy` process for the listed payloads, preserving their
relative paths beneath the destination prefix. It validates file types, sizes,
keys and multipart limits before launch. The file list is bounded to 16 MiB;
empty batches start no process. Restore uses the client only to fetch the index;
Model Streamer reads payloads using the sizes recorded there. See the
[restore contract](../../S3.md#layout-and-index) for limitations.
Metadata writes use `copyto`; reads use `lsjson`
and `cat` with exact file lists and `--no-traverse`, without listing the bucket.
A missing-object response is distinct from access denial. Object keys must be
canonical relative paths, since rclone normalizes paths; unsupported spellings
are rejected before any request. Wildcards and shell metacharacters in supported
keys are literal bytes.

Each operation uses `fork` and `execve`, with a fixed executable path, argument
array and controlled environment. There is no shell or persistent rclone server.
Callers must keep the source tree unchanged until the operation returns.
Concurrent checkpoints using the same source directory or S3 destination prefix
are unsupported; the client does not coordinate overlapping writers or detect
changes during upload. The child closes unrelated descriptors before exec.
Captured output is bounded and raw CLI diagnostics are never exposed to callers.
Command failures throw `S3Error`, carrying only the HTTP status extracted from
the pinned CLI format (zero when unavailable). Cancellation and deadlines throw
the shared `CancellationError` directly.

The daemon resolves credentials once at startup, as described in
[the S3 guide](../../S3.md#configuration-and-credentials). With a credentials
file, `S3Client` receives that snapshot and passes it to rclone as explicit
credentials. The file is bounded to 64 KiB, and Kubernetes Secret symlinks are
supported.

Without a credentials file, `S3Client` receives no credentials and starts
rclone with environment authentication. The child then runs the default
credential chain on the workload identity variables listed in the S3 guide,
which `S3Client` copies from the daemon's environment when it is constructed.
Those credentials refresh inside rclone. Model Streamer runs its own default
credential chain in the daemon process, with the same variables; see
[the S3 guide](../../S3.md#configuration-and-credentials).

Region and endpoint come from the storage JSON. The optional `AWS_CA_BUNDLE`
fallback must be absolute. Credential values never appear in arguments or logs.

`active_files` bounds concurrent CLI processes, including metadata operations.
A payload batch occupies one slot and transfers one file at a time. Independent
batches can run concurrently. Each process receives `buffer_budget / active_files`
through rclone's `--max-buffer-memory`, with up to four parts uploaded concurrently
and no additional transfer buffer. `part_size` sets both the multipart
threshold and chunk size. It defaults to 64 MiB, supporting files up to 625 GiB
within S3's fixed 10,000-part limit. Larger files require a larger configured
part size; oversized files are rejected before upload.
Each process's budget must fit one part rounded up to
the next whole MiB, including an extra MiB for an already aligned part. This lets
rclone finish reading a part before uploading it. The budget limits active
buffers; idle buffer caches, Go/TLS overhead, metadata, and file lists use
additional memory. The CRT throughput target has no replacement setting.
`request_retries + 1` sets rclone's low-level attempts; top-level command
retries are disabled. The pinned S3 backend always makes up to two pacer calls,
even with `request_retries = 0`. Failed metadata reads therefore make up to
`2 * (request_retries + 1)` HTTP attempts. Multipart operations can also be
replayed by the copy layer, up to `2 * (request_retries + 1)^2` attempts for a
persistently failing request across copies. Single-part PUT uses up to
`request_retries + 1` attempts. These are layered CLI retries, not CRT's exact
per-request retry count; the operation deadline bounds the whole sequence.
Connection and idle I/O timeouts are separate from the operation deadline.

Each call requires a caller-supplied deadline covering admission and execution;
the client has no separate overall timeout setting. Cancellation sends SIGTERM,
then SIGKILL after a two-second grace period if necessary, and
reaps the child before returning. Forced termination cannot guarantee remote
multipart cleanup. A failed write may have completed remotely; the client does
not track remote outcome or cleanup state. Callers confirm index publication by
reading back its exact bytes. Index publication remains separate from payload
uploads. HTTPS retains upstream integrity checks. Explicit HTTP endpoints use
rclone's unsigned-payload mode and do not provide TLS transport protection.

The component's `make test` runs subprocess, credential and local Moto regression
tests. The S3 fixture's `--resources` option also samples the parent and active
children during isolated upload, concurrent-upload and metadata probes. Local
emulation does not establish production AWS IAM or throughput behavior.
