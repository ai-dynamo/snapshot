// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

type fixtureEntry struct {
	Path      string `json:"path"`
	Mode      uint32 `json:"mode"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
}

type fixtureManifest struct {
	RootMode uint32         `json:"root_mode"`
	Bytes    int64          `json:"bytes"`
	Entries  []fixtureEntry `json:"entries"`
}

const manifestName = "manifest.yaml"

// Limit each read, and check cancellation even when the source is a local file.
func transfer(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func hashFile(ctx context.Context, path string) (string, error) {
	return readFile(ctx, path, "")
}

// Read once to hash, optionally copying the same bytes into an owned file.
func readFile(ctx context.Context, path, destination string) (digest string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var writer io.Writer = h
	if destination != "" {
		var out *os.File
		out, err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, out.Close()) }()
		writer = io.MultiWriter(out, h)
	}
	if err := transfer(ctx, writer, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validPath(path string) bool {
	if !utf8.ValidString(path) || len(path) > 512 || strings.Contains(path, "\\") || !fs.ValidPath(path) {
		return false
	}
	for _, c := range path {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

func inspect(ctx context.Context, root string) (fixtureManifest, error) {
	return scanFixture(ctx, root, "")
}

// Walk actual source entries to authorize paths; an optional destination copies
// and hashes in the same pass. Directory modes are applied after population.
func scanFixture(ctx context.Context, root, destination string) (fixtureManifest, error) {
	var m fixtureManifest
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := validateEntry(info); err != nil {
			return err
		}
		if path == root {
			m.RootMode = uint32(info.Mode().Perm())
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !validPath(rel) || len(m.Entries) >= 10000 {
			return errors.New("invalid fixture path or entry limit")
		}
		e := fixtureEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Directory: d.IsDir()}
		to := ""
		if destination != "" {
			to = filepath.Join(destination, rel)
		}
		if e.Directory {
			if to != "" {
				err = os.Mkdir(to, 0700)
			}
		} else {
			e.Size = info.Size()
			m.Bytes += e.Size
			if m.Bytes > 1<<40 {
				return errors.New("fixture exceeds 1 TiB")
			}
			e.SHA256, err = readFile(ctx, path, to)
			if err == nil && to != "" {
				err = os.Chmod(to, fs.FileMode(e.Mode))
			}
		}
		if err != nil {
			return err
		}
		m.Entries = append(m.Entries, e)
		return nil
	})
	if err != nil {
		return m, err
	}
	for _, e := range m.Entries {
		if e.Path == manifestName && !e.Directory && e.Size <= 1<<20 {
			return m, nil
		}
	}
	return m, errors.New("fixture requires regular manifest.yaml up to 1 MiB")
}

func validateEntry(info fs.FileInfo) error {
	mode := info.Mode()
	if mode & ^(fs.ModeDir|0777) != 0 {
		return errors.New("fixture contains unsupported type or mode")
	}
	if info.IsDir() {
		if mode.Perm()&0500 != 0500 {
			return errors.New("fixture directories require owner read and execute")
		}
	} else if mode.Perm()&0400 == 0 {
		return errors.New("fixture files require owner read")
	}
	return nil
}

func verify(ctx context.Context, path string, expected fixtureManifest) error {
	actual, err := inspect(ctx, path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("restored tree differs in paths, types, permissions, sizes or SHA-256")
	}
	return nil
}

func admittedManifest(ctx context.Context, r request) (fixtureManifest, error) {
	var m fixtureManifest
	digest, err := hashFile(ctx, r.FixtureManifest)
	if err != nil {
		return m, err
	}
	if digest != r.ManifestSHA256 {
		return m, errors.New("fixture manifest changed")
	}
	if err := readJSON(r.FixtureManifest, maxManifest, &m); err != nil {
		return m, err
	}
	// copyFixture walks the actual source and compares its metadata and the
	// copied bytes with this admission record before publication.
	return m, nil
}

func copyFixture(ctx context.Context, source, destination string, m fixtureManifest) error {
	actual, err := scanFixture(ctx, source, destination)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, m) {
		return errors.New("source corpus changed after admission")
	}
	return applyDirectoryModes(destination, m)
}

func applyDirectoryModes(root string, m fixtureManifest) error {
	for i := len(m.Entries) - 1; i >= 0; i-- {
		e := m.Entries[i]
		if e.Directory {
			if err := os.Chmod(filepath.Join(root, e.Path), fs.FileMode(e.Mode)); err != nil {
				return err
			}
		}
	}
	return os.Chmod(root, fs.FileMode(m.RootMode))
}

// Filesystem staged restore reserves top-level gpu/ for the separate GPU
// execution path. Nest a captured corpus so every original byte uses the CPU
// file-transfer path under test. S3 still requires a root manifest.yaml.
func wrappedManifest(source fixtureManifest) (fixtureManifest, error) {
	m := fixtureManifest{RootMode: 0700, Bytes: source.Bytes,
		Entries: make([]fixtureEntry, 0, len(source.Entries)+2)}
	m.Entries = append(m.Entries, fixtureEntry{Path: "corpus", Directory: true, Mode: source.RootMode})
	var manifest fixtureEntry
	for _, e := range source.Entries {
		if e.Path == manifestName {
			manifest = e
			m.Bytes += e.Size
		}
		e.Path = "corpus/" + e.Path
		if !validPath(e.Path) {
			return m, errors.New("wrapped corpus path exceeds native bounds")
		}
		m.Entries = append(m.Entries, e)
	}
	// Preserve WalkDir's directory-first traversal inside corpus, then its
	// next root sibling. A whole-path sort changes prefix-sibling ordering.
	m.Entries = append(m.Entries, manifest)
	if len(m.Entries) > 10000 || m.Bytes > 1<<40 {
		return m, errors.New("wrapped corpus exceeds native checkpoint bounds")
	}
	return m, nil
}

func copyCorpus(ctx context.Context, source, destination string, m fixtureManifest) (fixtureManifest, error) {
	wrapped, err := wrappedManifest(m)
	if err != nil {
		return wrapped, err
	}
	child := filepath.Join(destination, "corpus")
	if err := os.Mkdir(child, 0700); err != nil {
		return wrapped, err
	}
	if err := copyFixture(ctx, source, child, m); err != nil {
		return wrapped, err
	}
	manifest := filepath.Join(destination, manifestName)
	if _, err := readFile(ctx, filepath.Join(child, manifestName), manifest); err != nil {
		return wrapped, err
	}
	if err := os.Chmod(manifest, fs.FileMode(wrapped.Entries[len(wrapped.Entries)-1].Mode)); err != nil {
		return wrapped, err
	}
	if err := os.Chmod(destination, fs.FileMode(wrapped.RootMode)); err != nil {
		return wrapped, err
	}
	return wrapped, verify(ctx, destination, wrapped)
}

func generate(ctx context.Context, root string, r request) error {
	if err := os.MkdirAll(filepath.Join(root, "nested/empty-dir"), 0700); err != nil {
		return err
	}
	files := map[string][]byte{
		manifestName:       []byte(fmt.Sprintf("version: 1\nrun: %s\ncycle: %s\n", r.RunID, r.CycleID)),
		"nested/small.bin": {0, 255, 1, 128, 10, 0, 42}, "empty": {},
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0640); err != nil {
			return err
		}
	}
	seed := sha256.Sum256([]byte(r.RunID + "/" + r.CycleID))
	rng := rand.NewChaCha8(seed)
	f, err := os.OpenFile(filepath.Join(root, "nested/payload.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = transfer(ctx, f, io.LimitReader(rng, r.PayloadBytes))
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(root, "nested"), 0750); err != nil {
		return err
	}
	return os.Chmod(root, 0700)
}
