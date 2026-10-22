<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Model Streamer configuration

PageBroker accepts an optional `--model-streamer-config /path/streamer.json`
startup argument for native S3 reader tuning alongside
`--storage-config /path/s3.json`. Filesystem storage uses POSIX and does not use
Model Streamer. The former `filesystem` tuning group is rejected.
Both optional arguments may appear in either order. Existing invocations work
without a tuning file.

```sh
pagebroker /run/pagebroker.sock /staging /storage \
  --max-concurrent-requests 16 \
  --storage-config /config/s3.json \
  --model-streamer-config /config/streamer.json
```

The deployment owner supplies/mounts the files and passes these arguments.
PageBroker validates them and translates tuning into the pinned native C API
and process environment. Settings are fixed until restart; replacing a file
does not reload it. Credentials remain external and are resolved through the
existing [S3 configuration](S3.md#configuration-and-credentials).

## Input contract

Every field is optional; `{}` supplies no overrides. Files are bounded to 64 KiB.
Unknown fields, duplicate members, incorrect types and out-of-range values fail
startup. Diagnostics identify configuration fields without printing their values.

This example illustrates the schema, not recommended performance settings:

```json
{
  "readChunkBytes": 8388608,
  "processGroupSize": 1,
  "s3Reader": {
    "concurrency": 2,
    "targetGbps": 10,
    "maxConnections": 8,
    "maxInflightMiB": 128,
    "maxRetries": 3,
    "retryWindowSeconds": 0,
    "lowSpeedTimeoutMs": 2000,
    "lowSpeedBytesPerSecond": 1
  },
  "logging": {"level": "WARNING", "toStderr": true}
}
```

Native variable names below omit the common `RUNAI_STREAMER_` prefix.

| JSON field | Native setting | Accepted values |
| --- | --- | --- |
| `readChunkBytes` | `CHUNK_BYTESIZE` | Integer, 2 MiB–1 GiB; native S3 reads floor it to 5 MiB |
| `processGroupSize` | `PROCESS_GROUP_SIZE` | Positive unsigned 32-bit integer |
| `s3Reader.concurrency` | `OBJ_CONCURRENCY` | Integer, 1–1024 |
| `s3Reader.targetGbps` | `S3_TARGET_GBPS` | Positive unsigned 32-bit integer, Gbps per native client |
| `s3Reader.maxConnections` | `S3_MAX_CONNECTIONS` | Positive unsigned 32-bit integer, per native client |
| `s3Reader.maxInflightMiB` | `S3_MAX_INFLIGHT_MIB` | Integer, 1–1048576 MiB |
| `s3Reader.maxRetries` | `S3_MAX_RETRIES` | Integer, 0–10; **zero selects the CRT default** |
| `s3Reader.retryWindowSeconds` | `S3_TIMEOUT` | Integer, 0–86400; zero disables additional chunk retries |
| `s3Reader.lowSpeedTimeoutMs` | `S3_REQUEST_TIMEOUT_MS` | Integer, 1–86400000 ms |
| `s3Reader.lowSpeedBytesPerSecond` | `S3_LOW_SPEED_LIMIT` | Positive unsigned 32-bit integer |
| `logging.level` | `LOG_LEVEL` | `SPAM`, `DEBUG`, `INFO`, `WARNING`, `ERROR` |
| `logging.toStderr` | `LOG_TO_STDERR` | JSON boolean; native environment accepts `0` or `1` |

The bounds also apply to recognized inherited environment settings. Native
sentinel zeros for connections, throughput, inflight size and low-speed settings
are not accepted by this contract; omit a field/variable to use its fallback.

## Precedence and compatibility

For each tuning field: JSON overrides its native environment variable, then
PageBroker's existing S3 fallback applies where defined, otherwise the library
uses its default. The legacy `RUNAI_STREAMER_CONCURRENCY` supplies object-storage
concurrency only when its specific setting is absent. Explicit values are checked before any native environment mutation.

When S3 storage is configured, omitted reader settings retain these fallbacks:

| Reader field | Existing PageBroker fallback |
| --- | --- |
| `maxConnections` | 4, independent of upload throughput target |
| `maxInflightMiB` | 128, independent of CRT upload memory budget |
| `readChunkBytes` | Smaller of 16 MiB and `limits.uploadPartBytes` |
| `lowSpeedTimeoutMs` | `limits.requestSeconds * 1000` |
| `maxRetries` | 3 |
| `retryWindowSeconds` | 0 |

Explicit reader values no longer get overwritten by upload limits. Unlike older
versions, recognized native environment overrides also take precedence over
these S3 fallbacks. Shared S3 endpoint, region, credentials, addressing and CA
settings still come from the existing storage configuration/provider resolution.
Reader tuning does not change SDK upload limits.

## Native behavior

`readChunkBytes` controls object-storage reads. The native
inflight window is per client and does not cap total broker staging or SDK memory.
Low-speed timeout is a stall detector, not a total request deadline. The extra
retry window begins with the chunk's first submission and does not interrupt
an inflight request. Broker transaction deadlines still apply.

PageBroker applies process settings before constructing native workers and
validates them when replacing a failed restore coordinator. Direct C++
callers should resolve/apply `ModelStreamerOptions` once before constructing
engines; conflicting process options are rejected. Engines cannot independently
choose different process-wide profiles. Native session admission, destination
mapping lifetime and cancellation/draining rules remain in force.

GPU staging, registered-buffer controls and Python-only settings are not exposed
by this CPU destination configuration.
