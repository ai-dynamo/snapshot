<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3 client

`S3Client` runs [rclone v1.75.2](https://github.com/rclone/rclone/releases/tag/v1.75.2).
The PageBroker Dockerfile downloads the official prebuilt Linux amd64 executable
and verifies its release archive against a pinned SHA-256 checksum. It does not
compile or modify rclone. Matching sources and modules are downloaded only for
attribution. License information, dependency notices, archive checksums and binary
build information are packaged under `/legal/rclone`. PageBroker no longer builds
or links the AWS C++ SDK.
Model Streamer's prebuilt S3 plugin retains its own AWS implementation.

The client exposes `UploadFiles`, `Head`, bounded `Get`, and create-only `Put`.
`UploadFiles` runs one `copy` process for the listed payloads, preserving their
relative paths beneath the destination prefix. It validates file types, sizes,
keys and multipart limits before launch. The file list is bounded to 16 MiB;
empty batches start no process. Metadata writes use `copyto`; reads use `lsjson`
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

The daemon reads the `[default]` profile from `AWS_SHARED_CREDENTIALS_FILE` once
at startup. The file must contain `aws_access_key_id` and `aws_secret_access_key`;
`aws_session_token` is optional. Files are bounded to 64 KiB, and Kubernetes
Secret symlinks are supported. Missing, malformed or incomplete files fail
startup; environment access keys and named profiles cannot override or supplement
them. The daemon shares that snapshot with rclone and Model Streamer.
`S3Client` accepts only resolved connection settings and never discovers credentials.
Region and endpoint come from the storage JSON. The optional `AWS_CA_BUNDLE`
fallback must be absolute. Restart PageBroker to adopt changed credentials;
there is no automatic refresh. Credential values never appear in arguments or logs.

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
