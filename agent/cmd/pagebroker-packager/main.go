// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
)

func main() {
	checkpoint := flag.String("checkpoint", "", "CRIU checkpoint directory")
	checkpointID := flag.String("checkpoint-id", "", "checkpoint identity")
	flag.Parse()
	if *checkpoint == "" {
		fmt.Fprintln(os.Stderr, "--checkpoint is required")
		os.Exit(2)
	}
	manifest, err := pagebroker.GenerateManifest(*checkpoint, *checkpointID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s resident_bytes=%d objects=%d\n", pagebroker.ManifestFilename, manifest.ResidentBytes, len(manifest.HostMemoryObjects))
}
