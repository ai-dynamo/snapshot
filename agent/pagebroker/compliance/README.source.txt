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
The uploader and metadata client execute the unmodified rclone release:

  https://github.com/rclone/rclone/releases/tag/v1.75.2

The Dockerfile downloads the official Linux amd64 executable and pins its release
archive by SHA-256. It does not compile or modify rclone. The matching source
archive and module downloads are used only to collect attribution. /legal/rclone
contains the source archive, MIT license, Go license, module dependency notices,
executable build information and release/source archive checksums.
The PageBroker executable does not link the AWS C++ SDK. Model Streamer's
prebuilt S3 plugin still contains its own AWS implementation.
OpenSSL libcrypto remains available for downstream checkpoint hashing;
its attribution is in /legal/openssl/copyright. The JSON parser's attribution
is in /legal/nlohmann-json/copyright.

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

Model Streamer uses the native SDK release published at:

  https://github.com/dsx-ai-factory/model-streamer/releases/tag/0.17.0

Its release and pinned upstream build definition are also linked in
/legal/model-streamer/UPSTREAM.txt. Its release version, SDK checksum, source
revision and source archive are installed alongside that file.

Model Streamer selects and statically links its libaio and liburing dependencies
when building libstreamer.so. Snapshot consumes that prebuilt SDK; it does not
build or independently select those dependency versions. The source directory
also contains their matching source archives and license texts for attribution.
The upstream build definition records their versions and checksums.
