// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

const (
	// imageGrowthSampleInterval is how often the dump watcher stats the
	// image directory. The directory is a handful of files (one of them
	// large), so a stat walk is cheap next to the write itself.
	imageGrowthSampleInterval = 200 * time.Millisecond
	maxImageGrowthSamples     = 1000
)

// ImageGrowthSample is one stat of the CRIU image directory during a dump.
// Offset is measured from the start of the dump call.
type ImageGrowthSample struct {
	Offset time.Duration
	Bytes  int64
}

// DirectoryBytes sums regular files under root. Symlinks are not followed,
// so a CRIU parent link is not counted twice.
func DirectoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", root, err)
	}
	return total, nil
}

// sampleImageGrowth stats checkpointDir until stop is called. stop waits for
// the sampler to exit and returns every sample it kept, including one taken
// at stop. The caller must call stop exactly once.
func sampleImageGrowth(checkpointDir string) (stop func() []ImageGrowthSample) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []ImageGrowthSample, 1)
	go func() {
		samples := make([]ImageGrowthSample, 0, 32)
		start := time.Now()
		ticker := time.NewTicker(imageGrowthSampleInterval)
		defer ticker.Stop()
		record := func() {
			bytes, err := DirectoryBytes(checkpointDir)
			if err != nil {
				return
			}
			sample := ImageGrowthSample{
				Offset: time.Since(start),
				Bytes:  bytes,
			}
			// A long dump must keep observations across its whole span.
			// Thinning leaves the newest sample in place and frees room
			// for the next one, including the sample taken at stop.
			if len(samples) >= maxImageGrowthSamples {
				samples = thinGrowthSamples(samples)
			}
			samples = append(samples, sample)
		}
		for {
			record()
			select {
			case <-ctx.Done():
				record()
				done <- samples
				return
			case <-ticker.C:
			}
		}
	}()
	return func() []ImageGrowthSample {
		cancel()
		return <-done
	}
}

// thinGrowthSamples drops every other interior sample. The first and last
// observations stay, so the series still covers the dump after the sample cap.
func thinGrowthSamples(samples []ImageGrowthSample) []ImageGrowthSample {
	n := len(samples)
	if n < 3 {
		return samples
	}
	kept := make([]ImageGrowthSample, 0, n/2+1)
	kept = append(kept, samples[0])
	for i := 2; i < n-1; i += 2 {
		kept = append(kept, samples[i])
	}
	kept = append(kept, samples[n-1])
	return kept
}
