CORRESPONDING SOURCE — NVIDIA Dynamo Snapshot
================================================================================

This directory contains the complete corresponding source code for the
third-party open-source components redistributed in this container image.

It is provided to satisfy the source-availability obligations of the licenses
those components are distributed under, including the GNU GPL and LGPL, and to
meet NVIDIA container policy, which requires source for all open-source
binaries in a distributed image rather than only the copyleft ones.


LAYOUT
--------------------------------------------------------------------------------

  dpkg/
      Debian/Ubuntu source packages (.dsc + .orig + .debian tarballs) for every
      system package this image adds on top of its NVIDIA base container.

      DELTA.tsv    the exact package/version/source-package list this
                   directory corresponds to.
      SKIPPED.txt  packages with no public source repository. These are
                   NVIDIA-proprietary components from the CUDA repositories,
                   which are not open source and are governed by the NVIDIA
                   Software License Agreement instead.

  criu/
      criu-src.tar.gz — full CRIU source tree at the exact commit this image
      was built from. CRIU is licensed GPL-2.0, with the contents of its lib/
      directory under LGPL-2.1.

  go/vendor/
      Complete source for every Go module statically linked into the NVIDIA
      binaries in this image, as produced by `go mod vendor`. modules.txt
      records the exact module set and versions used by the build.


SCOPE
--------------------------------------------------------------------------------

This directory covers what this image adds on top of its NVIDIA base container.

Source for the base container's own contents is provided by NVIDIA through the
NGC channel for that base image, and is not duplicated here.

NVIDIA's own components — including cuda-checkpoint and the CUDA libraries in
the base image — are not open source and carry no source-availability
obligation. They are governed by the NVIDIA Software License Agreement and the
Product-Specific Terms for NVIDIA AI Products.

NVIDIA-authored open-source code in this image (the snapshot agent, nsrestore,
the cuda-checkpoint-helper, and the inet-remap CRIU plugin) is licensed
Apache-2.0 and published at https://github.com/ai-dynamo/snapshot.


ATTRIBUTION
--------------------------------------------------------------------------------

Per-component license texts for everything listed here are consolidated in
/legal/THIRD-PARTY.txt.
