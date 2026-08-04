Corresponding source
================================================================================

Upstream source for the third-party components redistributed in this image.

  dpkg/        Debian source packages for the system packages this image adds
               on top of its base image. DELTA.tsv lists exactly which, and
               SKIPPED.txt records any with no public source repository.
  criu/        CRIU source at the commit this image was built from.
  go/vendor/   Source for the Go modules linked into the binaries in this
               image; modules.txt records the exact module set.

Source for the base image's own contents is provided through the channel for
that base image and is not duplicated here.

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.
