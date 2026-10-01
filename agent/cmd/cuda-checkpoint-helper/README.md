# CUDA checkpoint helper

The helper's `--daemon` mode calls `cuInit` and retains every primary CUDA
context before READY. On CustomStorage-capable drivers it also initializes
the persistent host rings and NIXL registrations in either storage mode.

The daemon composes driver lifecycle, transfers and metadata without a
PageBroker dependency. Private inherited sockets carry admission, CUDA phases
and drain acknowledgements; session descriptors pin the artifact directory and
target namespace. The owner process supervises its lifetime. Legacy one-shot
CLI commands remain unchanged; the batch CLI and Go supervision follow in the
next PR.

`helper.cpp` handles CLI parsing and legacy dispatch. `daemon.hpp` exposes
`RunDaemon(control_fd, options)` for a dedicated GPU process. A future
PageBroker supervisor can reuse this entry point, namespace admission,
session state and private phase protocol while supplying its own storage
descriptors. Owner loss or uncertain CUDA cleanup retires only that GPU
process; this is not an entry point for the PageBroker CPU daemon.

Production builds require CUDA 13.4 headers and POSIX NIXL. Run `make helper
test CUDA_ROOT=/usr/local/cuda NIXL_ROOT=/usr/local` in this directory.
The default ring reserves 4 GiB per visible GPU; the optional pinned-memory
cap is checked before allocation. Checksums are disabled unless requested.
