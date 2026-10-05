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

This image is distroless and installs no system packages, so statically linked
protobuf is the only third-party content this image adds. The base image
contains third-party components of its own -- glibc, libstdc++ and libgcc,
which this binary links dynamically. Source for the base image's own contents
is published by NVIDIA and is not duplicated here:

  https://developer.download.nvidia.com/distroless-oss/cc/v4.0.8/

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.
