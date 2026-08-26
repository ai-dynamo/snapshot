// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

// paxXattrPrefix is the PAX record prefix GNU tar uses for extended
// attributes captured with --xattrs (CaptureRootfsDiff passes it).
const paxXattrPrefix = "SCHILY.xattr."

// extractBufferSize buffers sequential reads of the staged archive (1 MiB).
const extractBufferSize = 1 << 20

type entryOutcome int

const (
	entryExtracted entryOutcome = iota
	entrySkippedExisting
	entrySkippedUnsupported
)

// ExtractRootfsDiff extracts the tar archive at src into targetRoot.
//
// Only archives produced by CaptureRootfsDiff are supported: GNU tar output in
// GNU/PAX format with SCHILY.xattr extended-attribute records. Entries whose
// target path already exists are skipped (the diff contains only overlay
// upperdir changes; base-image files must never be overwritten — GNU tar's
// --skip-old-files semantics), while extracted
// entries get their mode, ownership, timestamps, and extended
// attributes restored. Entry paths are resolved with SecureJoin, so neither
// ".." components nor symlinks planted by earlier entries can escape
// targetRoot.
func ExtractRootfsDiff(src, targetRoot string, log logr.Logger) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open rootfs diff: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(bufio.NewReaderSize(f, extractBufferSize))
	var extracted, skippedExisting, skippedUnsupported int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read rootfs diff archive: %w", err)
		}
		outcome, err := extractEntry(tr, hdr, targetRoot, log)
		if err != nil {
			return fmt.Errorf("extract %q: %w", hdr.Name, err)
		}
		switch outcome {
		case entryExtracted:
			extracted++
		case entrySkippedExisting:
			skippedExisting++
		case entrySkippedUnsupported:
			skippedUnsupported++
		}
	}
	log.Info("Rootfs diff extracted",
		"entries", extracted,
		"skipped_existing", skippedExisting,
		"skipped_unsupported", skippedUnsupported,
	)
	return nil
}

func extractEntry(r io.Reader, hdr *tar.Header, targetRoot string, log logr.Logger) (entryOutcome, error) {
	target, err := securejoin.SecureJoin(targetRoot, hdr.Name)
	if err != nil {
		return 0, fmt.Errorf("resolve target path: %w", err)
	}

	if _, err := os.Lstat(target); err == nil {
		return entrySkippedExisting, nil
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("stat target: %w", err)
	}

	// The parent may not have its own archive entry, or its entry may have
	// been skipped as pre-existing on a path that does not exist in the
	// restore target (deleted base-image directory recreated in the upperdir).
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, fmt.Errorf("create parent directory: %w", err)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.Mkdir(target, 0o755); err != nil {
			return 0, err
		}
	case tar.TypeReg:
		if err := writeRegularFile(target, r); err != nil {
			return 0, err
		}
	case tar.TypeSymlink:
		// The link destination is deliberately not validated: dangling and
		// absolute symlinks are legitimate container content, and nothing in
		// this extractor follows them (SecureJoin resolves later entries that
		// traverse them back inside targetRoot).
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return 0, err
		}
	case tar.TypeLink:
		linkTarget, err := securejoin.SecureJoin(targetRoot, hdr.Linkname)
		if err != nil {
			return 0, fmt.Errorf("resolve hardlink target: %w", err)
		}
		// If the link target's own entry was skipped as pre-existing, this
		// links to the existing file — the same result tar produces.
		if err := os.Link(linkTarget, target); err != nil {
			return 0, err
		}
		// Metadata travels with the inode; the earlier entry set it.
		return entryExtracted, nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if err := makeSpecialFile(target, hdr); err != nil {
			return 0, err
		}
	default:
		log.V(1).Info("Skipping unsupported archive entry type",
			"name", hdr.Name, "typeflag", hdr.Typeflag)
		return entrySkippedUnsupported, nil
	}

	return entryExtracted, applyMetadata(target, hdr, log)
}

func writeRegularFile(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(target)
		return fmt.Errorf("write content: %w", err)
	}
	return f.Close()
}

func makeSpecialFile(target string, hdr *tar.Header) error {
	mode := uint32(hdr.Mode) & 0o7777
	switch hdr.Typeflag {
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	case tar.TypeFifo:
		mode |= unix.S_IFIFO
	}
	dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
	return unix.Mknod(target, mode, int(dev))
}

// applyMetadata restores mode, ownership, xattrs, and timestamps on an entry
// this extractor created. Ownership must precede chmod (chown clears setuid).
// Directory mtimes are not corrected after children are created inside them —
// the drift is cosmetic for a rootfs diff and not worth a second pass.
func applyMetadata(target string, hdr *tar.Header, log logr.Logger) error {
	// Only root restores ownership, mirroring tar's --same-owner default.
	// Restore runs as root; unprivileged callers are tests.
	if os.Geteuid() == 0 {
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}

	if hdr.Typeflag != tar.TypeSymlink {
		mode := hdr.FileInfo().Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}

	for key, value := range hdr.PAXRecords {
		if !strings.HasPrefix(key, paxXattrPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, paxXattrPrefix)
		if err := unix.Lsetxattr(target, name, []byte(value), 0); err != nil {
			// Some filesystems and user namespaces refuse specific xattr
			// namespaces; losing one is not worth failing the restore.
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
				log.V(1).Info("Skipping xattr the target refused",
					"path", target, "xattr", name, "error", err.Error())
				continue
			}
			return fmt.Errorf("set xattr %s: %w", name, err)
		}
	}

	return applyTimes(target, hdr)
}

func applyTimes(target string, hdr *tar.Header) error {
	if hdr.ModTime.IsZero() {
		return nil
	}
	mtime := unix.NsecToTimespec(hdr.ModTime.UnixNano())
	atime := mtime
	if !hdr.AccessTime.IsZero() {
		atime = unix.NsecToTimespec(hdr.AccessTime.UnixNano())
	}
	ts := []unix.Timespec{atime, mtime}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("set times: %w", err)
	}
	return nil
}
