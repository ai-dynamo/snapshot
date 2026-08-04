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

  go/vendor/
      Complete source for every Go module statically linked into the NVIDIA
      binaries in this image, as produced by `go mod vendor`. modules.txt
      records the exact module set and versions used by the build.


SCOPE
--------------------------------------------------------------------------------

This directory covers what this image adds on top of its NVIDIA base container.

Source for the base container's own contents is provided by NVIDIA through the
NGC channel for that base image, and is not duplicated here.

This image is distroless and installs no system packages over its base, so Go
modules are the whole of its third-party content.

NVIDIA-authored open-source code in this image (the operator manager and the
CRD installer) is licensed Apache-2.0 and published at
https://github.com/ai-dynamo/snapshot.


ATTRIBUTION
--------------------------------------------------------------------------------

Per-component license texts for everything listed here are consolidated in
/legal/THIRD-PARTY.txt.
