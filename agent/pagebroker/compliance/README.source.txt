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

This image also ships the pinned Model Streamer core native library, with its
SDK license and release metadata in /legal/model-streamer/.

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
