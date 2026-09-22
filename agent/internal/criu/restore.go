// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	criulib "github.com/checkpoint-restore/go-criu/v8"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// RestoreLogFilename is the CRIU restore log filename (also used by executor/restore.go).
const RestoreLogFilename = "restore.log"

const (
	netNsPath                    = "/proc/1/ns/net"
	placeholderFDDir             = "/proc/1/fd"
	restoreScratchTempDirPattern = "criu-restore-*"
)

var physicalNVIDIADeviceName = regexp.MustCompile(`^nvidia[0-9]+$`)

// ExecuteRestore opens the image/work directory FDs, configures inherited
// resources, and calls go-criu Restore. Returns the namespace-relative PID.
func ExecuteRestore(
	criuOpts *criurpc.CriuOpts,
	m *types.CheckpointManifest,
	checkpointPath string,
	bundleDir string,
	log logr.Logger,
) (int32, func() error, time.Duration, time.Duration, error) {
	settings := m.CRIUDump.CRIU
	var prepare, restore time.Duration

	// Return the FD closers as cleanup() rather than deferring them here, so the
	// caller can run them after cuda unlock instead of between the CRIU restore
	// and unlock. That keeps the window where the restored process runs with CUDA
	// still locked as short as possible. cleanup is called on the error paths below.
	var openFiles, inheritedFiles []*os.File
	scratchDir, removeScratch, err := restoreScratchDir(settings.WorkDir)
	if err != nil {
		return 0, nil, 0, 0, err
	}
	prepareStart := time.Now()
	imageDirPath, removeImageDir, err := prepareRestoreImageDir(checkpointPath, scratchDir)
	prepare = time.Since(prepareStart)
	if err != nil {
		removeScratch()
		return 0, nil, prepare, 0, fmt.Errorf("failed to prepare CRIU image directory: %w", err)
	}
	cleanup := func() error {
		closeFiles(inheritedFiles)
		closeFiles(openFiles)
		cleanupErr := removeImageDir()
		removeScratch()
		return cleanupErr
	}
	cleanupAfterError := func() {
		if err := cleanup(); err != nil {
			log.Error(err, "failed to clean CRIU restore resources")
		}
	}

	// Open image dir FD
	imageDir, imageDirFD, err := openPathForCRIU(imageDirPath)
	if err != nil {
		cleanupAfterError()
		return 0, nil, prepare, 0, fmt.Errorf("failed to open image directory: %w", err)
	}
	openFiles = append(openFiles, imageDir)
	criuOpts.ImagesDirFd = proto.Int32(imageDirFD)

	// Open work dir FD
	if settings.WorkDir != "" {
		workDirFile, workDirFD, err := openPathForCRIU(settings.WorkDir)
		if err != nil {
			cleanupAfterError()
			return 0, nil, prepare, 0, fmt.Errorf("failed to open CRIU work directory: %w", err)
		}
		openFiles = append(openFiles, workDirFile)
		criuOpts.WorkDirFd = proto.Int32(workDirFD)
	}

	overridePath, err := rewriteCRIULibDir(criuOpts.ConfigFile, scratchDir, bundleDir)
	if err != nil {
		cleanupAfterError()
		return 0, nil, prepare, 0, err
	}
	criuOpts.ConfigFile = proto.String(overridePath)

	criuBin, err := bundledCRIUPath(bundleDir)
	if err != nil {
		cleanupAfterError()
		return 0, nil, prepare, 0, err
	}
	c := criulib.MakeCriu()
	c.SetCriuPath(criuBin)

	netNsFile, err := os.Open(netNsPath)
	if err != nil {
		cleanupAfterError()
		return 0, nil, prepare, 0, fmt.Errorf("failed to open net NS at %s: %w", netNsPath, err)
	}
	openFiles = append(openFiles, netNsFile)
	c.AddInheritFd("extNetNs", netNsFile)

	inheritedFiles = registerInheritFDs(c, m.K8s.StdioFDs, log)

	notify := &restoreNotify{log: log}
	log.V(1).Info("Executing go-criu Restore call")
	restoreStart := time.Now()
	if err := c.Restore(criuOpts, notify); err != nil {
		restore = time.Since(restoreStart)
		log.Error(err, "go-criu Restore returned error")
		logging.LogRestoreErrors(imageDirPath, settings.WorkDir, log)
		cleanupAfterError()
		return 0, nil, prepare, restore, fmt.Errorf("CRIU restore failed: %w", err)
	}
	restore = time.Since(restoreStart)

	return notify.restoredPID, cleanup, prepare, restore, nil
}

// BuildRestoreOpts assembles CriuOpts for a CRIU restore from the checkpoint manifest.
// ImagesDirFd and WorkDirFd are left unset — ExecuteRestore opens them at restore time.
func BuildRestoreOpts(m *types.CheckpointManifest, checkpointPath string, cgroupRoot string, log logr.Logger) (*criurpc.CriuOpts, error) {
	extMounts, err := buildRestoreExtMounts(m)
	if err != nil {
		return nil, err
	}
	log.V(1).Info("Generated external mount map set", "ext_mount_count", len(extMounts))

	settings := m.CRIUDump.CRIU
	criuOpts := &criurpc.CriuOpts{
		LogFile: proto.String(RestoreLogFilename),
		Root:    proto.String("/"),
		ExtMnt:  extMounts,
	}
	if err := applyCommonSettings(criuOpts, &settings); err != nil {
		return nil, err
	}

	// Restore-only options
	criuOpts.RstSibling = proto.Bool(settings.RstSibling)
	criuOpts.MntnsCompatMode = proto.Bool(settings.MntnsCompatMode)
	criuOpts.EvasiveDevices = proto.Bool(settings.EvasiveDevices)
	criuOpts.ForceIrmap = proto.Bool(settings.ForceIrmap)

	// Skip network locking on restore. CRIU only locks the network on this
	// path via unlock_connection_info (criu/sk-tcp.c) and, under --empty-ns
	// net, network_lock_internal (criu/cr-restore.c:2176) — neither applies
	// here: the net namespace is external (see dump.go) and the restore target
	// is a fresh placeholder netns with no dump-time DROP rules in it. SKIP is
	// therefore inert, and it keeps criu from forking iptables-restore, which
	// the injected bundle does not carry.
	//
	// Do NOT mirror this into BuildDumpOpts. Dump is where lock_connection
	// actually protects the C/R window for tcpEstablished connections.
	criuOpts.NetworkLock = criurpc.CriuNetworkLockMethod_SKIP.Enum()

	if cgroupRoot != "" && shouldSetCgroupRoot(criuOpts.GetManageCgroupsMode()) {
		criuOpts.CgRoot = []*criurpc.CgroupRoot{
			{Path: proto.String(cgroupRoot)},
		}
	}

	criuConfPath := filepath.Join(checkpointPath, criuConfFilename)
	if _, err := os.Stat(criuConfPath); err == nil {
		criuOpts.ConfigFile = proto.String(criuConfPath)
	}

	return criuOpts, nil
}

func buildRestoreExtMounts(m *types.CheckpointManifest) ([]*criurpc.ExtMountMap, error) {
	if len(m.CRIUDump.ExtMnt) == 0 {
		return nil, fmt.Errorf("checkpoint manifest is missing criuDump.extMnt")
	}

	restoreMap := map[string]string{"/": "."}
	for _, val := range m.CRIUDump.ExtMnt {
		if val == "" || val == "/" {
			continue
		}
		restoreMap[val] = val
	}
	return toExtMountMaps(restoreMap), nil
}

// GPUDeviceMounts keeps the allocated devices pinned across CRIU's mount replay.
type GPUDeviceMounts struct {
	devices map[string]*os.File
	aliases []gpuMountAlias
}

type gpuMountAlias struct {
	path    string
	created bool
}

// Close releases the pinned devices and, unless committed, unwinds the aliases.
func (m *GPUDeviceMounts) Close(committed bool) error {
	var errs []error
	if !committed {
		// Unwind completed setup steps in reverse order, including partial
		// failures. Detach aliases before removing their mountpoint files.
		for _, alias := range slices.Backward(m.aliases) {
			if err := unix.Unmount(alias.path, unix.MNT_DETACH); err != nil {
				errs = append(errs, err)
				continue
			}
			if alias.created {
				errs = append(errs, os.Remove(alias.path))
			}
		}
	}
	m.aliases = nil
	for _, f := range m.devices {
		errs = append(errs, f.Close())
	}
	m.devices = nil
	return errors.Join(errs...)
}

// PrepareGPUDeviceMounts applies the inspected physical mount aliases.
// Pin every destination before modifying paths: mappings can overlap or cycle.
func PrepareGPUDeviceMounts(aliases map[string]string, log logr.Logger) (_ *GPUDeviceMounts, retErr error) {
	m := &GPUDeviceMounts{devices: make(map[string]*os.File)}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, m.Close(false))
		}
	}()
	for path, target := range aliases {
		if !isPhysicalNVIDIADevicePath(path) || !isPhysicalNVIDIADevicePath(target) {
			return nil, fmt.Errorf("invalid GPU mount alias %q -> %q", path, target)
		}
		if m.devices[target] != nil {
			return nil, fmt.Errorf("duplicate destination GPU path %s", target)
		}
		f, err := os.OpenFile(target, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("pin destination device %s: %w", target, err)
		}
		m.devices[target] = f
		info, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat destination GPU path %s: %w", target, err)
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			return nil, fmt.Errorf("destination GPU path %s is not a character device", target)
		}
	}
	for path, target := range aliases {
		created := false
		if info, err := os.Lstat(path); os.IsNotExist(err) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			_ = f.Close()
			created = true
		} else if err != nil {
			return nil, err
		} else if info.Mode()&os.ModeCharDevice == 0 {
			return nil, fmt.Errorf("checkpoint GPU path %s is not a character device", path)
		}
		source := fmt.Sprintf("/proc/self/fd/%d", m.devices[target].Fd())
		if err := unix.Mount(source, path, "", unix.MS_BIND, ""); err != nil {
			if created {
				err = errors.Join(err, os.Remove(path))
			}
			return nil, err
		}
		m.aliases = append(m.aliases, gpuMountAlias{path: path, created: created})
		log.Info("Aliased GPU device mount", "checkpoint_device", path, "restore_device", target)
	}
	return m, nil
}

// RestoreNativePaths puts allocated devices at their native paths before CUDA
// restore. CRIU replays checkpoint mounts, which can hide destination-only paths
// under a new /dev. Native paths also take precedence over temporary aliases in
// overlapping mappings once CRIU has consumed those aliases.
func (m *GPUDeviceMounts) RestoreNativePaths(pid int) error {
	if len(m.devices) == 0 {
		return nil
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return err
	}
	defer ns.Close()
	root, err := os.Open(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return err
	}
	defer root.Close()
	devFD, err := unix.Openat(int(root.Fd()), "dev", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open restored /dev for pid %d: %w", pid, err)
	}
	defer unix.Close(devFD)

	// Detached mounts can cross namespaces; ordinary bind mounts from the
	// placeholder namespace cannot. Clone from the pins, never the aliased paths.
	mounts := make(map[string]int)
	defer func() {
		for _, fd := range mounts {
			_ = unix.Close(fd)
		}
	}()
	for path, f := range m.devices {
		fd, err := unix.OpenTree(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			return fmt.Errorf("clone GPU mount %s: %w", path, err)
		}
		mounts[path] = fd
	}
	done := make(chan error, 1)
	go func() {
		// Mount-namespace setns requires private filesystem state. Do not unlock:
		// Go terminates this thread on return rather than reusing its namespace.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			done <- err
			return
		}
		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNS); err != nil {
			done <- err
			return
		}
		for path, fd := range mounts {
			if err := installGPUDeviceMount(devFD, filepath.Base(path), fd); err != nil {
				done <- fmt.Errorf("restore native GPU path %s for pid %d: %w", path, pid, err)
				return
			}
		}
		done <- nil
	}()
	return <-done
}

func installGPUDeviceMount(devFD int, name string, mountFD int) error {
	if err := unix.Mknodat(devFD, name, unix.S_IFREG|0o600, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	targetFD, err := unix.Openat(devFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	var st unix.Stat_t
	if err := unix.Fstat(targetFD, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fmt.Errorf("GPU mountpoint is not a regular file or character device")
	}
	return unix.MoveMount(mountFD, "", targetFD, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
}

// GPUMountAliases derives the path mapping once for compatibility inspection and
// mount preparation. Checkpoints without UUID/path metadata keep their old behavior.
func GPUMountAliases(m *types.CheckpointManifest, deviceMap string, targets map[string]string) (map[string]string, error) {
	if len(m.CUDA.DevicePaths) == 0 {
		return nil, nil
	}
	mapping := map[string]string{}
	for _, uuid := range m.CUDA.SourceGPUUUIDs {
		mapping[uuid] = uuid
	}
	if deviceMap != "" {
		for _, pair := range strings.Split(deviceMap, ",") {
			source, target, ok := strings.Cut(pair, "=")
			_, knownSource := mapping[source]
			if !ok || !knownSource || target == "" {
				return nil, fmt.Errorf("invalid CUDA device mapping %q", pair)
			}
			mapping[source] = target
		}
	}
	aliases := map[string]string{}
	seenSource := map[string]bool{}
	seenTarget := map[string]bool{}
	for sourceUUID, sourcePath := range m.CUDA.DevicePaths {
		if !isPhysicalNVIDIADevicePath(sourcePath) {
			return nil, fmt.Errorf("invalid source GPU device path %q", sourcePath)
		}
		targetPath := targets[mapping[sourceUUID]]
		if !isPhysicalNVIDIADevicePath(targetPath) {
			return nil, fmt.Errorf("missing destination device path for source GPU %s", sourceUUID)
		}
		if seenSource[sourcePath] || seenTarget[targetPath] {
			return nil, fmt.Errorf("GPU mount mapping is not one-to-one at %s", sourcePath)
		}
		seenSource[sourcePath], seenTarget[targetPath] = true, true
		if _, external := m.CRIUDump.ExtMnt[sourcePath]; external && sourcePath != targetPath {
			aliases[sourcePath] = targetPath
		}
	}
	for path := range m.CRIUDump.ExtMnt {
		if !isPhysicalNVIDIADevicePath(path) {
			continue
		}
		found := false
		for _, sourcePath := range m.CUDA.DevicePaths {
			found = found || sourcePath == path
		}
		if !found {
			return nil, fmt.Errorf("checkpoint GPU mount %s has no UUID metadata", path)
		}
	}
	return aliases, nil
}

func isPhysicalNVIDIADevicePath(path string) bool {
	return path == filepath.Clean(path) && filepath.Dir(path) == "/dev" && physicalNVIDIADeviceName.MatchString(filepath.Base(path))
}

func registerInheritFDs(c *criulib.Criu, stdioFDs []string, log logr.Logger) []*os.File {
	if len(stdioFDs) == 0 {
		log.V(1).Info("No stdio FD descriptors in manifest, skipping inherit-fd setup")
		return nil
	}

	var openFiles []*os.File
	for i, target := range stdioFDs {
		if !strings.Contains(target, "pipe:") {
			continue
		}
		// stdin (fd 0) is a read-end pipe; stdout/stderr (fd 1, 2) are write-end
		openMode := os.O_WRONLY
		if i == 0 {
			openMode = os.O_RDONLY
		}
		fdPath := fmt.Sprintf("%s/%d", placeholderFDDir, i)
		f, err := os.OpenFile(fdPath, openMode, 0)
		if err != nil {
			log.V(1).Info("Failed to open placeholder stdio FD, skipping", "fd", i, "target", target, "error", err)
			continue
		}
		openFiles = append(openFiles, f)
		c.AddInheritFd(target, f)
	}

	log.V(1).Info("Registered inherited stdio pipes", "count", len(openFiles))
	return openFiles
}

// restoreScratchDir is the local parent for per-restore CRIU files that must
// not live on the checkpoint PVC: criu-restore.conf and criu-restore-images-*.
// When workDir is set (typically /var/criu-work) that durable directory is
// used and not removed. When it is unset, a criu-restore-* temp is created.
func restoreScratchDir(workDir string) (string, func(), error) {
	if workDir != "" {
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return "", nil, fmt.Errorf("failed to create CRIU work directory: %w", err)
		}
		return workDir, func() {}, nil
	}
	dir, err := os.MkdirTemp("", restoreScratchTempDirPattern)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create CRIU restore scratch directory: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// rewriteCRIULibDir writes a criu.conf override that redirects the plugin libdir
// to the injected bundle. The dump-time libdir points to the agent filesystem and
// is unreachable inside the placeholder namespace; the override replaces it.
// existingConfigFile is nil when the checkpoint was dumped without a criu.conf;
// in that case the override is written from an empty base so CRIU always finds
// its plugins at the injected path. scratchDir is the restore scratch from
// restoreScratchDir (workDir or a criu-restore-* temp).
func rewriteCRIULibDir(existingConfigFile *string, scratchDir, criuBundleDir string) (string, error) {
	if scratchDir == "" {
		return "", fmt.Errorf("CRIU restore scratch directory is empty")
	}

	var baseConf string
	if existingConfigFile != nil {
		data, err := os.ReadFile(*existingConfigFile)
		if err != nil {
			return "", fmt.Errorf("read criu config %s: %w", *existingConfigFile, err)
		}
		baseConf = string(data)
	}

	overridePath := filepath.Join(scratchDir, "criu-restore.conf")
	conf := overrideLibDir(baseConf, filepath.Join(criuBundleDir, "criu-plugins"))
	if err := os.WriteFile(overridePath, []byte(conf), 0644); err != nil {
		return "", fmt.Errorf("write criu libdir override to %s: %w", overridePath, err)
	}
	return overridePath, nil
}

// bundledCRIUPath returns the path to the criu binary inside bundleDir.
// BinaryPath in the checkpoint manifest refers to the agent filesystem and is
// unusable inside the placeholder namespace; criu must always come from the injected bundle.
func bundledCRIUPath(bundleDir string) (string, error) {
	path := filepath.Join(bundleDir, "criu")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("criu binary not found in injected bundle at %s: %w", path, err)
	}
	return path, nil
}

func overrideLibDir(conf, libDir string) string {
	lines := strings.Split(conf, "\n")
	replaced := false
	for i, line := range lines {
		if isLibDirLine(line) {
			lines[i] = "libdir " + libDir
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, "libdir "+libDir)
	}
	return strings.Join(lines, "\n")
}

func isLibDirLine(line string) bool {
	// CRIU's config parser (criu/config.c) accepts both space and tab as the
	// key/value separator, so match both to avoid a duplicate libdir directive.
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "libdir ") || strings.HasPrefix(trimmed, "libdir\t")
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			file.Close()
		}
	}
}

type restoreNotify struct {
	criulib.NoNotify
	restoredPID int32
	log         logr.Logger
}

func (n *restoreNotify) PreRestore() error {
	n.log.V(1).Info("CRIU pre-restore")
	return nil
}

func (n *restoreNotify) PostRestore(pid int32) error {
	n.restoredPID = pid
	n.log.Info("CRIU post-restore: process restored", "pid", pid)
	return nil
}
