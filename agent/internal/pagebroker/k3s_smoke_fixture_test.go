// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type smokeEntry struct {
	Directory bool
	Mode      fs.FileMode
	Size      int64
	SHA256    string
}

// Write in bounded chunks so the client does not allocate a checkpoint-sized buffer.
func writeSmokeFixture(ctx context.Context, directory string, payloadBytes int64, seed string) error {
	for _, relative := range []string{"nested", "nested/empty-directory"} {
		if err := os.Mkdir(filepath.Join(directory, relative), 0750); err != nil {
			return err
		}
	}
	for relative, content := range map[string][]byte{
		"manifest.yaml": []byte("version: 1\nsmoke: " + seed + "\n"),
		"nested/small":  []byte("checkpoint\x00payload\xff\n" + seed),
		"empty":         {},
	} {
		if err := os.WriteFile(filepath.Join(directory, relative), content, 0600); err != nil {
			return err
		}
	}
	if err := writeSmokePayload(ctx, filepath.Join(directory, "nested/data.bin"), payloadBytes, seed); err != nil {
		return err
	}
	for relative, mode := range map[string]fs.FileMode{
		".": 0750, "nested": 0751, "nested/empty-directory": 0500,
		"manifest.yaml": 0640, "nested/data.bin": 0600, "nested/small": 0400, "empty": 0644,
	} {
		if err := os.Chmod(filepath.Join(directory, relative), mode); err != nil {
			return err
		}
	}
	return nil
}

func writeSmokePayload(ctx context.Context, path string, size int64, seed string) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	salt := sha256.Sum256([]byte(seed))
	chunk := make([]byte, 1<<20)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		data := chunk[:min(int64(len(chunk)), size-offset)]
		for index := range data {
			position := offset + int64(index)
			data[index] = byte(position*31+position/251) ^ salt[position%int64(len(salt))]
		}
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		offset += int64(n)
	}
	return nil
}

func readSmokeTree(directory string) (map[string]smokeEntry, error) {
	entries := make(map[string]smokeEntry)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		value := smokeEntry{Directory: info.IsDir(), Mode: info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unexpected non-regular fixture path: %s", relative)
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			digest := sha256.New()
			value.Size, err = io.Copy(digest, file)
			if err := errors.Join(err, file.Close()); err != nil {
				return err
			}
			value.SHA256 = hex.EncodeToString(digest.Sum(nil))
		}
		entries[filepath.ToSlash(relative)] = value
		return nil
	})
	return entries, err
}

func verifySmokeTree(directory string, expected map[string]smokeEntry) error {
	actual, err := readSmokeTree(directory)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("restored tree has %d entries, expected %d", len(actual), len(expected))
	}
	for path, want := range expected {
		if got, found := actual[path]; !found || got != want {
			return fmt.Errorf("restored path %s differs: got %+v, expected %+v", path, got, want)
		}
	}
	return nil
}

func validateSmokeStaging(root, directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("PageBroker returned a non-canonical staging path")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("PageBroker staging path is outside the configured staging root")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("PageBroker staging path is not a directory")
	}
	return nil
}

func smokeRemoved(directory string) error {
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("expected staging or unpublished destination to be absent: %s (stat: %v)", directory, err)
	}
	return nil
}
