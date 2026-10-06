Corresponding source
====================

Source for the third-party components distributed in this image:

  protobuf/    Debian source package for the statically linked protobuf library.
               Its version matches the installed development package.
  libaio/      Debian source package for the bundled libaio runtime library.
               Its version matches the installed binary package.
  nixl/        NIXL and its dependencies at the revision built for POSIX I/O.
               VERSION records the NIXL commit.
  nvtx/        NVTX headers and notices at the profiling revision.
               VERSION records the NVTX commit.
  pagebroker/  NVIDIA daemon source, including the GPU engine. Build output and
               generated protobuf files are excluded. Run `make generate` to
               generate protobuf files from the supplied .proto file.

NVIDIA publishes source for the distroless base image at:

  https://developer.download.nvidia.com/distroless-oss/cc/v4.0.8/

The NVIDIA container runtime supplies the host CUDA driver library. This image
excludes the CUDA driver stub used at build time.

NVIDIA-authored code uses the Apache-2.0 license and is published at:

  https://github.com/ai-dynamo/snapshot

Component license texts are in /legal/THIRD-PARTY.txt.
