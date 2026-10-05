Corresponding source
================================================================================

Upstream source for the third-party components redistributed in this image.

  protobuf/    Debian source package (.dsc, upstream tarball, Debian diff) for
               protobuf, statically linked into /usr/local/bin/pagebroker, at
               the source version the linked libprotobuf.a was built from.
               VERSION records that source version.

  pagebroker/  NVIDIA-authored source for the daemon itself, as built. Build
               artifacts and protoc-generated files are excluded; `make
               generate` reproduces the latter from the shipped .proto.

This image also ships the pinned Model Streamer core and S3 native libraries,
with their wheel licenses and metadata in /legal/model-streamer/ and
/legal/model-streamer-s3/. OpenSSL libcrypto provides SHA-256; its attribution
is in /legal/openssl/copyright. The base image
contains third-party components of its own -- glibc, libstdc++ and libgcc,
which this binary links dynamically. Source for the base image's own contents
is published by NVIDIA and is not duplicated here:

  https://developer.download.nvidia.com/distroless-oss/cc/v4.0.8/

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.

Model Streamer is pinned to ae93548e02be46f479e7689a7e10e9bb243bb653.
Its native wheel metadata, source revision, and source archive are installed
under /legal/model-streamer/. The source directory also contains the exact
libaio 0.3.113 and liburing 2.14 source archives (including their license texts)
used by the pinned upstream toolchain to build libstreamer.so.
