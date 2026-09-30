# CUDA checkpoint helper

The existing CLI and its driver actions, process queries, and launch-job
handling are unchanged. This foundation simplifies the unused CustomStorage
contracts before a persistent transfer implementation is added.

`storage_manifest.*` owns version-4 extent metadata and GPU mapping.
`content_digest.*` and `extent_digests.*` keep optional SHA-256 metadata outside
the core manifest. The generic transfer interface, unavailable backend, layout
planner, and per-operation allocation limits are retired.

The Helm `cudaCheckpoint` settings declare the optional persistent helper,
capture storage mode, buffer allocation, and `enableChecksumDigest: false`.
They are wired into agent execution later in the stack. PageBroker's protocol
and module layout stay unchanged.

Run `make -C agent/cmd/cuda-checkpoint-helper test-storage` for the storage and
checksum tests. These require a C++20 compiler and OpenSSL development files.
