Corresponding source
================================================================================

Upstream source for the third-party components redistributed in this image.

  dpkg/        Debian source packages for the system packages this image adds
               on top of its base image, each pinned to the source version its
               binary was built from. DELTA.tsv lists exactly which.
               SKIPPED.txt records any package whose source was not fetched,
               with the reason. A build only succeeds when every such entry is
               a package published without source by an NVIDIA repository; any
               other fetch failure fails the build.
  criu/        CRIU source at the commit this image was built from.
  go/vendor/   Source for the Go modules linked into the binaries in this
               image; modules.txt records the exact module set.

CuInterpose frontend, core, coordinator, and launcher sources are in
/legal/cuinterpose/source. Every vendored Rust dependency, the standard-library
sources, and the pinned toolchain's license notices are in /legal/cuinterpose/rust.
Cargo.lock records the exact dependency set, including build/test and other-target
sources that are not linked into the Linux runtime. NOTICES.txt contains each
vendored component's license texts and notices plus Rust's runtime inventory, and
is included in /legal/THIRD-PARTY.txt. The builder pins Rust 1.95.0 by container
digest and records its version in toolchain-version.txt. Keeping the matching
standard-library source is part of this image's source-bundling policy.

Source for the base image's own contents is published by NVIDIA and is not
duplicated here:

  https://developer.download.nvidia.com/assets/dlfw/
  (cuda_dl_base-25.11-cuda13.0-devel-ubuntu24.04.tar)

NVIDIA-authored code in this image is Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.

Per-component license texts are in /legal/THIRD-PARTY.txt.
