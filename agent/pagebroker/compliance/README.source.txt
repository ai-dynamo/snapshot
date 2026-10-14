Corresponding source
====================

Source for PageBroker and the third-party components distributed in this image:

  protobuf/    Debian source package for the statically linked protobuf library.
               Its version matches the installed development package.
  libaio/      Debian source package for the bundled libaio runtime library.
               Its version matches the installed binary package.
  nixl/        NIXL and its dependencies at the revision built for POSIX I/O.
               VERSION records the upstream NIXL commit.
  nvtx/        NVTX headers and notices at the profiling revision.
               VERSION records the NVTX commit.
  pagebroker/  NVIDIA daemon source, including the GPU engine. Build output and
               generated protobuf files are excluded. Run `make generate` to
               generate protobuf files from the supplied .proto file.

This image also ships the pinned Model Streamer core and S3 native libraries,
with their SDK license and release metadata in /legal/model-streamer/.
The S3 plugin license is also in /legal/model-streamer-s3/.
OpenSSL libcrypto provides SHA-256; its attribution
is in /legal/openssl/copyright. The uploader uses the S3 CRT client from AWS SDK for C++ 1.11.584,
source commit bba3cfc14d4fc148aeee7a8ff7822dd7a9a0f4d3:

  https://github.com/aws/aws-sdk-cpp/tree/bba3cfc14d4fc148aeee7a8ff7822dd7a9a0f4d3

The SDK build applies patches/aws-sdk-crt-cancellation.patch to retain requests
until queued cancellation finishes. The patch is also in /legal/aws-sdk/.
The SDK and its pinned CRT submodule notices are in /legal/aws-sdk/. Its
dynamically linked HTTP/TLS dependencies come from Ubuntu 24.04 packages;
their copyright files are in /legal/packages/. Package copyright files identify
their upstream sources and license terms. These dependencies are copied from
the same build stage as the SDK, not from the host.

The base image
contains third-party components of its own -- glibc, libstdc++ and libgcc,
which this binary links dynamically. Source for the base image's own contents
is published by NVIDIA and is not duplicated here:

  https://developer.download.nvidia.com/distroless-oss/cc/v4.0.8/

The NVIDIA container runtime supplies the host CUDA driver library. This image
excludes the CUDA driver stub used at build time.

NVIDIA-authored code uses the Apache-2.0 license and is published at:

  https://github.com/ai-dynamo/snapshot

Per-component license texts are in /legal/THIRD-PARTY.txt.

Model Streamer uses the 0.17.0 native SDK release, source revision
dd9e5d34f966cc2667c9f0505262050b4bedc35a.
Its release version, SDK checksum, source revision and source archive are installed
under /legal/model-streamer/. The source directory also contains the exact
libaio 0.3.113 and liburing 2.14 source archives (including their license texts)
used by the release toolchain to build libstreamer.so.
