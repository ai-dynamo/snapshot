// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
)

// The directory of the embedded server's file store, which holds the control
// buckets. A process owns its directory for as long as it holds a lock on the
// directory's lock file, so a directory whose lock is free belongs to a process
// that is gone.
const (
	// storeDirPrefix starts the name of every store directory, under the
	// temporary directory.
	storeDirPrefix = "snapshot-coordinator-"

	// storeLockName is the lock file of a store directory.
	storeLockName = "lock"

	// storeDirTries is how often newStoreDir makes a directory again when
	// another process's cleanup took it before it was locked.
	storeDirTries = 3
)

// storeDir is the directory a Server stores its control buckets in. Nothing
// outlives the process: remove deletes it, and a directory a crashed process
// leaves behind is deleted by the next one that starts.
type storeDir struct {
	path string
	lock *os.File
}

// newStoreDir creates and locks a store directory, and removes the ones that
// crashed processes left behind.
func newStoreDir(log logr.Logger) (*storeDir, error) {
	var err error
	for range storeDirTries {
		var path string
		if path, err = os.MkdirTemp("", storeDirPrefix); err != nil {
			return nil, fmt.Errorf("create the JetStream directory: %w", err)
		}
		var lock *os.File
		if lock, err = lockDir(path); err == nil {
			removeStaleStoreDirs(log)
			return &storeDir{path: path, lock: lock}, nil
		}
		_ = os.RemoveAll(path)
	}
	return nil, fmt.Errorf("lock the JetStream directory: %w", err)
}

// remove deletes the directory and releases its lock.
func (d *storeDir) remove() {
	_ = os.RemoveAll(d.path)
	_ = d.lock.Close()
}

// removeStaleStoreDirs removes every store directory whose process is gone. A
// directory that is locked, or that is not a directory, is left alone.
func removeStaleStoreDirs(log logr.Logger) {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		log.Error(err, "Listing the temporary directory for stale JetStream directories failed")
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), storeDirPrefix) {
			continue
		}
		path := filepath.Join(tmp, e.Name())
		lock, err := lockDir(path)
		if err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Error(err, "Removing a stale JetStream directory failed", "directory", path)
		} else {
			log.Info("Removed a JetStream directory that a stopped coordinator left behind", "directory", path)
		}
		_ = lock.Close()
	}
}

// lockDir takes the lock of the store directory path, which fails if another
// holder has it, or if the directory was removed by then.
func lockDir(path string) (*os.File, error) {
	name := filepath.Join(path, storeLockName)
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	// A cleanup that removed the directory after the file was opened leaves a
	// lock on a file nobody can reach.
	held, err := f.Stat()
	if err == nil {
		var named os.FileInfo
		if named, err = os.Stat(name); err == nil && !os.SameFile(held, named) {
			err = os.ErrNotExist
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
