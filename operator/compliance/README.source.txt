Corresponding source
================================================================================

Upstream source for the third-party components redistributed in this image.

  go/vendor/   Source for the Go modules linked into the binaries in this
               image; modules.txt records the exact module set.

This image is distroless and installs no system packages, so Go modules are its
only third-party content. Source for the base image's own contents is provided
through the channel for that base image and is not duplicated here.

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.
