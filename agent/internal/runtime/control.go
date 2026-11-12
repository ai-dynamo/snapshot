// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// WriteControlSentinel writes a sentinel file into the workload container's
// snapshot-control volume at SnapshotControlMountPath/<name>, accessed through
// the agent's /host/proc/<pid>/root view of the container's mount namespace.
//
// hostPID must be a PID inside the container's mount namespace (the container
// task PID is the canonical choice). The sentinel is observed by the workload
// via inotify on the control directory; it replaces the SIGUSR1/SIGCONT
// agent-to-workload signals that previously required the workload to run as
// PID 1.
//
// It writes through a handle on the control directory
// (openContainerControlDir), with WriteFileInDir.
func WriteControlSentinel(hostPID int, name string, contents []byte) error {
	return writeControlSentinel(HostProcPath, hostPID, name, contents)
}

func writeControlSentinel(procRoot string, hostPID int, name string, contents []byte) error {
	dir, err := openContainerControlDir(procRoot, hostPID)
	if err != nil {
		return fmt.Errorf("control sentinel %q: %w", name, err)
	}
	defer dir.Close()
	return WriteFileInDir(dir, name, 0o644, func(w io.Writer) error {
		_, err := w.Write(contents)
		return err
	})
}

// openContainerControlDir opens the snapshot-control directory of the
// container that hostPID runs in, through procRoot/<pid>/root, resolving the
// path inside the container's root (RESOLVE_IN_ROOT).
func openContainerControlDir(procRoot string, hostPID int) (*os.File, error) {
	if hostPID <= 0 {
		return nil, fmt.Errorf("invalid host PID %d", hostPID)
	}
	rootPath := filepath.Join(procRoot, strconv.Itoa(hostPID), "root")
	rootFD, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open container root %s: %w", rootPath, err)
	}
	defer unix.Close(rootFD)
	rel := strings.TrimPrefix(podcontract.SnapshotControlMountPath, "/")
	fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s in container root %s: %w", podcontract.SnapshotControlMountPath, rootPath, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(rootPath, rel)), nil
}

// KubeletPodsDir is kubelet's per-Pod directory, seen through the node's root
// filesystem: the agent runs with hostPID, so /host/proc/1 is the node's init.
// Unlike a hostPath mount it is not rewritten by vCluster, and it sees
// emptyDirs that kubelet mounts after the agent started, such as
// medium: Memory.
const KubeletPodsDir = HostProcPath + "/1/root/var/lib/kubelet/pods"

// podControlVolumeDir is the Pod's snapshot-control emptyDir under podsDir. It
// also checks containerName, which callers open inside it.
func podControlVolumeDir(podsDir, podUID, containerName string) (string, error) {
	for _, part := range []string{podUID, containerName} {
		if part == "" || part != filepath.Base(part) || part == ".." {
			return "", fmt.Errorf("invalid control directory part %q", part)
		}
	}
	return filepath.Join(podsDir, podUID, "volumes", "kubernetes.io~empty-dir",
		podcontract.SnapshotControlVolumeName), nil
}

// OpenPodControlDir opens a container's snapshot-control directory as seen
// from the node: the container's subPath directory inside the Pod's control
// emptyDir, created if kubelet has not. Unlike the /host/proc/<pid>/root view,
// it needs no process in the container, so it works before the container
// starts and after its process is gone. Write into it with WriteFileInDir.
//
// The volume directory belongs to kubelet, but everything inside it can be
// changed by the Pod. So the container's directory is opened without
// following symlinks, and files are created relative to it, never by path.
func OpenPodControlDir(podUID, containerName string) (*os.File, error) {
	return openPodControlDir(KubeletPodsDir, podUID, containerName)
}

func openPodControlDir(podsDir, podUID, containerName string) (*os.File, error) {
	volumePath, err := podControlVolumeDir(podsDir, podUID, containerName)
	if err != nil {
		return nil, err
	}
	volume, err := os.OpenFile(volumePath, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer volume.Close()
	vfd := int(volume.Fd())
	if err := unix.Mkdirat(vfd, containerName, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create control directory %s/%s: %w", volumePath, containerName, err)
	}
	fd, err := unix.Openat(vfd, containerName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open control directory %s/%s: %w", volumePath, containerName, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(volumePath, containerName)), nil
}

// WriteFileInDir writes name in dir: write fills a new file under a random
// name, which is then renamed over name. Readers never see a partial file, and
// the rename replaces whatever name was, without following it.
func WriteFileInDir(dir *os.File, name string, perm os.FileMode, write func(io.Writer) error) (retErr error) {
	if name == "" || name != filepath.Base(name) || name == ".." {
		return fmt.Errorf("invalid file name %q", name)
	}
	dfd := int(dir.Fd())
	var tmp string
	var fd int
	for {
		tmp = "." + name + "." + rand.Text()
		var err error
		fd, err = unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, uint32(perm))
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create %s in %s: %w", tmp, dir.Name(), err)
		}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), tmp))
	defer func() {
		if retErr != nil {
			_ = unix.Unlinkat(dfd, tmp, 0)
		}
	}()
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	// The umask may have narrowed perm at creation.
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(dfd, tmp, dfd, name); err != nil {
		return fmt.Errorf("rename %s to %s in %s: %w", tmp, name, dir.Name(), err)
	}
	return nil
}

// WritePodControlSentinel writes a sentinel into OpenPodControlDir.
func WritePodControlSentinel(podUID, containerName, name string, contents []byte) error {
	dir, err := OpenPodControlDir(podUID, containerName)
	if err != nil {
		return err
	}
	defer dir.Close()
	return WriteFileInDir(dir, name, 0o644, func(w io.Writer) error {
		_, err := w.Write(contents)
		return err
	})
}

// ReadPodControlFile reads name from OpenPodControlDir. The Pod can write
// anything there, so name is opened without following it, must be a regular
// file, and is refused when larger than maxBytes. A missing volume or file is
// an error that matches os.ErrNotExist.
func ReadPodControlFile(podUID, containerName, name string, maxBytes int64) ([]byte, error) {
	return readPodControlFile(KubeletPodsDir, podUID, containerName, name, maxBytes)
}

func readPodControlFile(podsDir, podUID, containerName, name string, maxBytes int64) ([]byte, error) {
	if name == "" || name != filepath.Base(name) || name == ".." {
		return nil, fmt.Errorf("invalid file name %q", name)
	}
	dir, err := openPodControlDir(podsDir, podUID, containerName)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// O_NONBLOCK keeps a FIFO from holding the open until a writer comes.
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s in %s: %w", name, dir.Name(), err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.Name())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name(), err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", f.Name(), maxBytes)
	}
	return data, nil
}

// RemovePodControlFile removes name from OpenPodControlDir, without following
// it. A file or volume that is already gone is not an error.
func RemovePodControlFile(podUID, containerName, name string) error {
	return removePodControlFile(KubeletPodsDir, podUID, containerName, name)
}

func removePodControlFile(podsDir, podUID, containerName, name string) error {
	if name == "" || name != filepath.Base(name) || name == ".." {
		return fmt.Errorf("invalid file name %q", name)
	}
	dir, err := openPodControlDir(podsDir, podUID, containerName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	return RemoveFileInDir(dir, name)
}

// RemoveFileInDir removes name from dir, without following it. A file that is
// already gone is not an error.
func RemoveFileInDir(dir *os.File, name string) error {
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s in %s: %w", name, dir.Name(), err)
	}
	return nil
}

// PodControlSentinelExists reports whether a sentinel exists in
// OpenPodControlDir, which needs no process in the container. A volume that
// kubelet has not set up holds none. Only a regular file counts; anything
// else under the name is an error, so callers do not mistake it for a
// missing sentinel.
func PodControlSentinelExists(podUID, containerName, name string) (bool, error) {
	return podControlSentinelExists(KubeletPodsDir, podUID, containerName, name)
}

func podControlSentinelExists(podsDir, podUID, containerName, name string) (bool, error) {
	if name == "" || name != filepath.Base(name) || name == ".." {
		return false, fmt.Errorf("invalid control sentinel name %q", name)
	}
	dir, err := openPodControlDir(podsDir, podUID, containerName)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("control sentinel %q: %w", name, err)
	}
	defer dir.Close()
	return SentinelInDirExists(dir, name)
}

// SentinelInDirExists reports whether dir holds a sentinel under name. Only a
// regular file counts; anything else under the name is an error.
func SentinelInDirExists(dir *os.File, name string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("stat control sentinel %s in %s: %w", name, dir.Name(), err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("control sentinel %s in %s is not a regular file", name, dir.Name())
	}
	return true, nil
}

// RemoveControlSentinel removes a sentinel from the snapshot-control mount at
// dir. The directory must exist: a missing file is already removed, but a
// missing mount is an error so callers do not confuse "cannot see the volume"
// with "volume is clean".
func RemoveControlSentinel(dir, name string) error {
	return removeSentinelInDir(dir, name)
}

func removeSentinelInDir(dir, name string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("control sentinel dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("control sentinel dir %s: not a directory", dir)
	}
	path := filepath.Join(dir, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove control sentinel %s: %w", path, err)
	}
	return nil
}
