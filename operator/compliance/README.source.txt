Corresponding source
================================================================================

Upstream source for the third-party components redistributed in this image.

  go/vendor/   Source for the Go modules linked into the binaries in this
               image; modules.txt records the exact module set.

This image is distroless and installs no system packages, so Go modules are the
only third-party content this image adds. The base image contains third-party
components of its own, covered by the source location below. Source for the base image's own contents is published
by NVIDIA and is not duplicated here:

  https://developer.download.nvidia.com/distroless-oss/go/v4.0.8/

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.
