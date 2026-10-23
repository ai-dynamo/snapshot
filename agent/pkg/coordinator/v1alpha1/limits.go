// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatorv1alpha1

// MaxMessageSize is the largest message either side of a stream must accept,
// which coordinator.proto states in rule 10 of Attach: a CompareAndSwap
// carrying two values of the largest size Snapshot's components may store,
// 32 MiB each, with room for its key and the rest of the request. A gRPC server
// passes it to grpc.MaxRecvMsgSize, and a client to grpc.MaxCallRecvMsgSize and
// grpc.MaxCallSendMsgSize. It lives beside the generated code because the
// agent's client and the coordinator's server both import this package, and no
// other.
const MaxMessageSize = 2*(32<<20) + 1<<20
