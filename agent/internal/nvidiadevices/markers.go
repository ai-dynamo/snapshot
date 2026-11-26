// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvidiadevices defines the NVIDIA device plugin's allocation marker paths.
package nvidiadevices

const (
	// MarkerDir is the directory selected by the volume-mounts device list strategy.
	MarkerDir = "/var/run/nvidia-container-devices"
	// MarkerDirAlias is MarkerDir under the common /var/run -> /run alias.
	MarkerDirAlias = "/run/nvidia-container-devices"
)
