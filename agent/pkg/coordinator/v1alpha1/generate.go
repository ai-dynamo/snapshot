// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinatorv1alpha1 is the wire protocol between Snapshot's agents and
// the coordinator: the messages and the Coordinator service generated from
// coordinator.proto, whose comments specify the protocol, and what both sides
// must agree on beyond the messages: the largest message, and the counter
// encoding.
package coordinatorv1alpha1

//go:generate protoc --proto_path=../.. --go_out=../.. --go_opt=module=github.com/ai-dynamo/snapshot/agent/pkg --go-grpc_out=../.. --go-grpc_opt=module=github.com/ai-dynamo/snapshot/agent/pkg ../../coordinator/v1alpha1/coordinator.proto
