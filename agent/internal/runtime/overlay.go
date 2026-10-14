// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	rootfsDiffFilename   = "rootfs-diff.tar"
	deletedFilesFilename = "deleted-files.json"
)

// GetRootFS returns the container's root filesystem path via /host/proc.
func GetRootFS(pid int) (string, error) {
	rootPath := fmt.Sprintf("%s/%d/root", HostProcPath, pid)
	if _, err := os.Stat(rootPath); err != nil {
		return "", fmt.Errorf("rootfs not accessible at %s: %w", rootPath, err)
	}
	return rootPath, nil
}

// GetOverlayUpperDir extracts the overlay upperdir from mountinfo.
func GetOverlayUpperDir(pid int) (string, error) {
	mountInfo, err := ReadMountInfo(pid)
	if err != nil {
		return "", fmt.Errorf("failed to parse mountinfo: %w", err)
	}

	for _, mount := range mountInfo {
		if mount.MountPoint != "/" || mount.FSType != "overlay" {
			continue
		}

		for _, opt := range strings.Split(mount.VFSOptions, ",") {
			if strings.HasPrefix(opt, "upperdir=") {
				return strings.TrimPrefix(opt, "upperdir="), nil
			}
		}
	}

	return "", fmt.Errorf("overlay upperdir not found for pid %d", pid)
}

// RootfsDiffOptions selects what CaptureRootfsDiff archives from the upperdir.
type RootfsDiffOptions struct {
	// Exclusions are tar patterns left out of the archive.
	Exclusions types.OverlaySettings
	// BindMountDests are the container's bind mount destinations, left out of
	// the archive.
	BindMountDests []string
	// Keep lists container paths, such as criu.ReadReferencedPaths returns,
	// that are archived even when an exclusion matches them.
	Keep []string
}

// RootfsDiff describes a captured rootfs diff.
type RootfsDiff struct {
	// Path is the published archive.
	Path string
	// Kept lists the Keep paths that the exclusions left out and that were
	// appended to the archive.
	Kept []string
	// KeptBytes is the total size of the regular files in Kept.
	KeptBytes int64
}

// ErrKeptFilesMissing marks a CaptureRootfsDiff failure that may have left a
// Keep path out of the rootfs diff, because the path is in the upperdir or
// could not be inspected there.
var ErrKeptFilesMissing = errors.New("rootfs diff may lack files that must be kept")

// CaptureRootfsDiff captures the overlay upperdir to a tar file.
//
// Files and directories in opts.Keep that are in the upperdir are kept even
// when they match an exclusion. Keep paths missing from the upperdir, or that
// would be reached through a symlink there, are skipped. Once a Keep path is
// found in the upperdir or cannot be inspected, any failure wraps
// ErrKeptFilesMissing.
//
// The archive is written to a temporary file first and renamed atomically on
// success, so a partial or failed capture never leaves a corrupt archive at the
// final path.
func CaptureRootfsDiff(upperDir, checkpointDir string, opts RootfsDiffOptions) (RootfsDiff, error) {
	if upperDir == "" {
		return RootfsDiff{}, fmt.Errorf("upperdir is empty")
	}
	keep, err := upperDirEntries(upperDir, opts.Keep)
	if err != nil {
		return RootfsDiff{}, fmt.Errorf("%w: %w", ErrKeptFilesMissing, err)
	}
	diff, err := writeRootfsDiff(upperDir, checkpointDir, opts, keep)
	if err != nil && len(keep) > 0 {
		return RootfsDiff{}, fmt.Errorf("%w: %w", ErrKeptFilesMissing, err)
	}
	return diff, err
}

// writeRootfsDiff archives the upperdir with the exclusions in opts, appends
// the keep entries the exclusions left out, and publishes the archive in
// checkpointDir.
func writeRootfsDiff(upperDir, checkpointDir string, opts RootfsDiffOptions, keep map[string]int64) (RootfsDiff, error) {
	rootfsDiffPath := filepath.Join(checkpointDir, rootfsDiffFilename)

	tmpFile, err := os.CreateTemp(checkpointDir, rootfsDiffFilename+".*.tmp")
	if err != nil {
		return RootfsDiff{}, fmt.Errorf("failed to create temporary rootfs diff: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return RootfsDiff{}, fmt.Errorf("failed to close temporary rootfs diff: %w", err)
	}
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	cmd := exec.Command("tar", captureTarArgs(upperDir, tmpPath, opts.Exclusions, opts.BindMountDests)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return RootfsDiff{}, fmt.Errorf("tar failed: %w (output: %s)", err, string(output))
	}

	diff := RootfsDiff{Path: rootfsDiffPath}
	if len(keep) > 0 {
		archived, err := archiveMembers(tmpPath)
		if err != nil {
			return RootfsDiff{}, err
		}
		members, appended := unarchivedMembers(slices.Sorted(maps.Keys(keep)), archived)
		if len(members) > 0 {
			cmd := exec.Command("tar", appendTarArgs(upperDir, tmpPath)...)
			cmd.Stdin = strings.NewReader(strings.Join(members, "\x00") + "\x00")
			if output, err := cmd.CombinedOutput(); err != nil {
				return RootfsDiff{}, fmt.Errorf("tar append of kept files failed: %w (output: %s)", err, string(output))
			}
		}
		for _, rel := range appended {
			diff.Kept = append(diff.Kept, "/"+rel)
			diff.KeptBytes += keep[rel]
		}
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return RootfsDiff{}, fmt.Errorf("failed to stat temporary rootfs diff: %w", err)
	}
	if info.Size() == 0 {
		return RootfsDiff{}, fmt.Errorf("tar produced empty rootfs diff")
	}

	if err := os.Rename(tmpPath, rootfsDiffPath); err != nil {
		return RootfsDiff{}, fmt.Errorf("failed to publish rootfs diff: %w", err)
	}
	tmpPath = ""

	return diff, nil
}

// captureTarArgs builds the archive-creation arguments for a rootfs diff.
//
// trusted.* is excluded because the archive is cut from the overlay upperdir:
// it carries trusted.overlay.opaque markers, and re-applying one to the
// restored container hides the base-image content under it.
func captureTarArgs(upperDir, outPath string, exclusions types.OverlaySettings, bindMountDests []string) []string {
	args := []string{"--xattrs", "--xattrs-exclude=trusted.*"}
	for _, excl := range buildExclusions(exclusions) {
		args = append(args, "--exclude="+excl)
	}
	for _, dest := range bindMountDests {
		args = append(args, "--exclude=."+dest)
	}
	return append(args, "-C", upperDir, "-cf", outPath, ".")
}

// appendTarArgs builds the arguments that append the NUL-separated members
// read from stdin to an archive captureTarArgs created, with the same xattr
// handling and without descending into directories.
func appendTarArgs(upperDir, outPath string) []string {
	return []string{
		"--xattrs", "--xattrs-exclude=trusted.*", "--no-recursion",
		"-C", upperDir, "-rf", outPath, "--null", "--verbatim-files-from", "-T", "-",
	}
}

// upperDirEntries returns the paths that are regular files or directories in
// the upperdir, relative to it, with the size of each regular file. A path is
// skipped when an upperdir ancestor is not a directory, so archiving it by name
// never resolves through a symlink. Any error other than a missing path is
// returned rather than read as absence.
func upperDirEntries(upperDir string, paths []string) (map[string]int64, error) {
	entries := make(map[string]int64)
	for _, p := range paths {
		p = filepath.Clean(p)
		if !filepath.IsAbs(p) || p == "/" {
			continue
		}
		rel := strings.TrimPrefix(p, "/")
		info, err := lstatUnderDirectories(upperDir, rel)
		if err != nil {
			return nil, err
		}
		if info == nil {
			continue
		}
		switch {
		case info.Mode().IsRegular():
			entries[rel] = info.Size()
		case info.IsDir():
			entries[rel] = 0
		}
	}
	return entries, nil
}

// lstatUnderDirectories returns the Lstat result for rel in dir, or nil when rel
// is missing or one of its ancestors is not a directory.
func lstatUnderDirectories(dir, rel string) (os.FileInfo, error) {
	current := dir
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("inspect /%s in the upperdir: %w", rel, err)
		}
		if i == len(parts)-1 {
			return info, nil
		}
		if !info.IsDir() {
			return nil, nil
		}
	}
	return nil, nil
}

// archiveMembers returns the member names of the tar archive at path, without
// their "./" prefix or a trailing slash. Names come from the headers, so any
// byte a name holds survives.
func archiveMembers(path string) (map[string]struct{}, error) {
	archive, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rootfs diff: %w", err)
	}
	defer archive.Close()
	// tar.Reader seeks over member data in an *os.File instead of reading it.
	reader := tar.NewReader(archive)
	members := make(map[string]struct{})
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read rootfs diff members: %w", err)
		}
		members[strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")] = struct{}{}
	}
}

// unarchivedMembers returns the members to append so every kept path is in the
// archive: each kept path the archive lacks, preceded by its ancestors the
// archive lacks. Ancestors are archived too so newly created directories can
// retain their modes and owners. appended holds the kept paths among them.
func unarchivedMembers(kept []string, archived map[string]struct{}) (members, appended []string) {
	queued := maps.Clone(archived)
	for _, rel := range kept {
		parts := strings.Split(rel, "/")
		for i := range parts {
			name := strings.Join(parts[:i+1], "/")
			if _, ok := queued[name]; ok {
				continue
			}
			queued[name] = struct{}{}
			members = append(members, "./"+name)
			if name == rel {
				appended = append(appended, rel)
			}
		}
	}
	return members, appended
}

// buildExclusions merges exclusion lists and normalizes paths for tar --exclude patterns.
func buildExclusions(s types.OverlaySettings) []string {
	exclusions := append([]string(nil), s.Exclusions...)
	for i, p := range exclusions {
		if strings.HasPrefix(p, "*") {
			continue
		}
		p = strings.TrimPrefix(p, ".")
		p = strings.TrimPrefix(p, "/")
		exclusions[i] = "./" + p
	}
	return exclusions
}

// CaptureDeletedFiles finds whiteout files and saves them to a JSON file.
func CaptureDeletedFiles(upperDir, checkpointDir string) (bool, error) {
	if upperDir == "" {
		return false, nil
	}

	whiteouts, err := findWhiteoutFiles(upperDir)
	if err != nil {
		return false, fmt.Errorf("failed to find whiteout files: %w", err)
	}

	if len(whiteouts) == 0 {
		return false, nil
	}

	deletedFilesPath := filepath.Join(checkpointDir, deletedFilesFilename)
	data, err := json.Marshal(whiteouts)
	if err != nil {
		return false, fmt.Errorf("failed to marshal whiteouts: %w", err)
	}

	if err := os.WriteFile(deletedFilesPath, data, 0644); err != nil {
		return false, fmt.Errorf("failed to write deleted files: %w", err)
	}

	return true, nil
}

// OverlayDiffBytes is the size of the rootfs diff and deleted-files list
// written into checkpointDir. Missing files contribute nothing.
func OverlayDiffBytes(checkpointDir string) int64 {
	return regularFileSize(filepath.Join(checkpointDir, rootfsDiffFilename)) +
		regularFileSize(filepath.Join(checkpointDir, deletedFilesFilename))
}

func regularFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

// restoreTarCmd builds the extraction command for a rootfs diff.
//
// The bundled tar runs under the bundled loader, named explicitly so the
// kernel never consults its PT_INTERP and resolves libc from the placeholder.
// See the bundle layout comment in internal/nsmount for why that is necessary
// and why the loader's glibc is kept out of lib/.
//
// bundleDir must be absolute. There is deliberately no PATH fallback: resolving
// "tar" inside the placeholder would find the placeholder's own tar, which is
// the failure this function exists to prevent.
func restoreTarCmd(bundleDir string, args ...string) (*exec.Cmd, error) {
	if !filepath.IsAbs(bundleDir) {
		return nil, fmt.Errorf("bundle dir must be an absolute path, got %q", bundleDir)
	}
	libcDir := filepath.Join(bundleDir, nsmount.BundleLibcDir)
	argv := append([]string{
		"--library-path", libcDir + ":" + filepath.Join(bundleDir, nsmount.BundleLibDir),
		filepath.Join(bundleDir, nsmount.BundleTar),
	}, args...)
	return exec.Command(filepath.Join(libcDir, nsmount.BundleLoader), argv...), nil
}

// ApplyRootfsDiff extracts rootfs-diff.tar into the target root.
//
// The archive is copied to local disk first. tar walks members with many small
// reads; doing that directly from NFS is much slower than one sequential copy
// plus a local extract.
func ApplyRootfsDiff(checkpointPath, targetRoot, bundleDir string, log logr.Logger) error {
	rootfsDiffPath := filepath.Join(checkpointPath, rootfsDiffFilename)
	info, err := os.Stat(rootfsDiffPath)
	if os.IsNotExist(err) {
		log.V(1).Info("No rootfs-diff.tar, skipping")
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to stat rootfs diff: %w", err)
	}
	if info.Size() == 0 {
		log.V(1).Info("rootfs-diff.tar is empty, skipping")
		return nil
	}

	localPath, cleanup, err := stageRootfsDiffLocally(rootfsDiffPath)
	if err != nil {
		return err
	}
	defer cleanup()

	// --skip-old-files: silently skip files that already exist in the restore target.
	// The rootfs diff only contains overlay upperdir changes (runtime-generated files
	// like triton caches, tmp files) — base image files should not be overwritten.
	// --numeric-owner: the restored processes hold the checkpoint's numeric ids;
	// re-resolving the archived names through the placeholder's passwd database
	// would remap them.
	// --xattrs-include: tar's extract mask defaults to user.*, so file
	// capabilities would be archived and then dropped. The list is enumerated
	// rather than widened to '*': that would also reapply security.selinux,
	// which can fail the restore on an SELinux node, and the trusted.overlay.*
	// markers carried by archives captured before they were excluded.
	log.Info("Applying rootfs diff", "target", targetRoot, "bytes", info.Size())
	cmd, err := restoreTarCmd(bundleDir,
		"--xattrs", "--xattrs-include=user.*", "--xattrs-include=security.capability",
		"--numeric-owner", "--skip-old-files", "--blocking-factor=2048",
		"-C", targetRoot, "-xf", localPath)
	if err != nil {
		return err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar extract failed: %w", err)
	}
	return nil
}

func stageRootfsDiffLocally(src string) (string, func(), error) {
	in, err := os.Open(src)
	if err != nil {
		return "", nil, fmt.Errorf("failed to open rootfs diff: %w", err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp("", rootfsDiffFilename+".*.tmp")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create local rootfs diff: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("failed to stage rootfs diff locally: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to close local rootfs diff: %w", err)
	}
	return tmpPath, cleanup, nil
}

// ApplyDeletedFiles removes files marked as deleted in the checkpoint.
func ApplyDeletedFiles(checkpointPath, targetRoot string, log logr.Logger) error {
	deletedFilesPath := filepath.Join(checkpointPath, deletedFilesFilename)
	data, err := os.ReadFile(deletedFilesPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read deleted files: %w", err)
	}

	var deletedFiles []string
	if err := json.Unmarshal(data, &deletedFiles); err != nil {
		return fmt.Errorf("failed to parse deleted files: %w", err)
	}

	count := 0
	targetRootAbs, err := filepath.Abs(targetRoot)
	if err != nil {
		return fmt.Errorf("failed to resolve target root %s: %w", targetRoot, err)
	}
	targetRootPrefix := targetRootAbs + string(os.PathSeparator)
	for _, f := range deletedFiles {
		if f == "" {
			continue
		}
		target := filepath.Join(targetRoot, f)
		targetAbs, err := filepath.Abs(target)
		if err != nil || (targetAbs != targetRootAbs && !strings.HasPrefix(targetAbs, targetRootPrefix)) {
			log.V(1).Info("Skipping out-of-root deleted file entry", "entry", f)
			continue
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			log.V(1).Info("Could not stat deleted file target", "path", target, "error", err)
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			log.V(1).Info("Could not delete file", "path", target, "error", err)
			continue
		}
		count++
	}
	log.Info("Deleted files applied", "count", count)
	return nil
}

// findWhiteoutFiles finds overlay whiteout files in the upperdir.
func findWhiteoutFiles(upperDir string) ([]string, error) {
	var whiteouts []string

	err := filepath.Walk(upperDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		name := info.Name()
		if strings.HasPrefix(name, ".wh.") {
			relPath, err := filepath.Rel(upperDir, path)
			if err != nil {
				return fmt.Errorf("failed to compute relative path for %s: %w", path, err)
			}
			dir := filepath.Dir(relPath)
			deletedFile := strings.TrimPrefix(name, ".wh.")
			deletedPath := deletedFile
			if dir != "." {
				deletedPath = filepath.Join(dir, deletedFile)
			}
			whiteouts = append(whiteouts, deletedPath)
		}
		return nil
	})

	return whiteouts, err
}
