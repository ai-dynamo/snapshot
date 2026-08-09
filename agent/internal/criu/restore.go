// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	criulib "github.com/checkpoint-restore/go-criu/v8"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// RestoreLogFilename is the CRIU restore log filename (also used by executor/restore.go).
const RestoreLogFilename = "restore.log"

const (
	netNsPath        = "/proc/1/ns/net"
	placeholderFDDir = "/proc/1/fd"
)

const (
	swrkTransportFD  = 3
	firstInheritedFD = swrkTransportFD + 1
)

// ExecuteRestore opens the image/work directory FDs, configures inherited
// resources, and runs CRIU through its swrk transport. Returns the namespace-relative PID.
func ExecuteRestore(
	criuOpts *criurpc.CriuOpts,
	m *types.CheckpointManifest,
	checkpointPath string,
	criuBinaryFD int,
	imageFD int,
	workFD int,
	providerFD *os.File,
	preResume func(int32) error,
	log logr.Logger,
) (int32, func(), error) {
	settings := m.CRIUDump.CRIU

	// Return the FD closers as cleanup() rather than deferring them here, so the
	// caller can run them after cuda unlock instead of between the CRIU restore
	// and unlock. That keeps the window where the restored process runs with CUDA
	// still locked as short as possible. cleanup is called on the error paths below.
	var openFiles, inheritedFiles []*os.File
	cleanup := func() {
		closeFiles(inheritedFiles)
		closeFiles(openFiles)
	}

	// Open image dir FD
	var imageDir *os.File
	var err error
	if imageFD >= 0 {
		imageDir, err = os.Open(fmt.Sprintf("/proc/self/fd/%d", imageFD))
	} else {
		imageDir, _, err = openPathForCRIU(checkpointPath)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("failed to open image directory: %w", err)
	}
	openFiles = append(openFiles, imageDir)

	// Open work dir FD
	var workDirFile *os.File
	if workFD >= 0 {
		workDirFile, err = os.Open(fmt.Sprintf("/proc/self/fd/%d", workFD))
		if err != nil {
			cleanup()
			return 0, nil, fmt.Errorf("failed to open inherited PageBroker work directory: %w", err)
		}
		openFiles = append(openFiles, workDirFile)
	} else if settings.WorkDir != "" {
		if err := os.MkdirAll(settings.WorkDir, 0755); err != nil {
			cleanup()
			return 0, nil, fmt.Errorf("failed to create CRIU work directory: %w", err)
		}
		workDirFile, _, err = openPathForCRIU(settings.WorkDir)
		if err != nil {
			cleanup()
			return 0, nil, fmt.Errorf("failed to open CRIU work directory: %w", err)
		}
		openFiles = append(openFiles, workDirFile)
	}

	var criuBinary *os.File
	if criuBinaryFD >= 0 {
		criuBinary, err = os.Open(fmt.Sprintf("/proc/self/fd/%d", criuBinaryFD))
		if err != nil {
			cleanup()
			return 0, nil, fmt.Errorf("open inherited CRIU executable: %w", err)
		}
		openFiles = append(openFiles, criuBinary)
	} else if _, err := os.Stat(settings.BinaryPath); err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("criu binary not found at %s: %w", settings.BinaryPath, err)
	}
	netNsFile, err := os.Open(netNsPath)
	if err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("failed to open net NS at %s: %w", netNsPath, err)
	}
	openFiles = append(openFiles, netNsFile)
	layout := restoreFDLayout{opts: criuOpts}
	criuOpts.ImagesDirFd = proto.Int32(int32(imageDir.Fd()))
	if workDirFile != nil {
		criuOpts.WorkDirFd = proto.Int32(int32(workDirFile.Fd()))
	}
	if providerFD != nil {
		layout.add("0-extmem-provider", providerFD)
	}
	layout.add("extNetNs", netNsFile)
	stdio := registerInheritFDs(m.K8s.StdioFDs, log)
	for _, fd := range stdio {
		layout.add(fd.key, fd.file)
		inheritedFiles = append(inheritedFiles, fd.file)
	}
	if providerFD != nil {
		inheritedFiles = append(inheritedFiles, providerFD)
	}
	if err := layout.validate(); err != nil {
		cleanup()
		return 0, nil, err
	}
	log.Info("Prepared CRIU swrk FD layout",
		"transport_fd", swrkTransportFD,
		"rpc_image_fd", criuOpts.GetImagesDirFd(),
		"rpc_work_fd", criuOpts.GetWorkDirFd(),
		"provider_fd", layout.fd("0-extmem-provider"),
	)

	notify := &restoreNotify{log: log, preResume: preResume}
	log.V(1).Info("Executing CRIU swrk restore")
	if err := restoreWithSWRK(settings.BinaryPath, criuBinary, criuOpts, layout.files, notify); err != nil {
		log.Error(err, "go-criu Restore returned error")
		if copiedPath, copyErr := copyRestoreLog(checkpointPath, workFD, settings.WorkDir); copyErr != nil {
			log.Error(copyErr, "Failed to copy CRIU restore log from namespace work directory")
		} else if copiedPath != "" {
			log.Info("Copied failed CRIU restore log to shared checkpoint", "path", copiedPath)
		}
		logging.LogRestoreErrors(checkpointPath, settings.WorkDir, log)
		cleanup()
		return 0, nil, fmt.Errorf("CRIU restore failed: %w", err)
	}
	if os.Getenv("SNAPSHOT_PRESERVE_CRIU_RESTORE_LOG") != "" {
		if copiedPath, copyErr := copyRestoreLogTo(checkpointPath, workFD, settings.WorkDir, RestoreLogFilename+".completed"); copyErr != nil {
			log.Error(copyErr, "Failed to preserve completed CRIU restore log")
		} else if copiedPath != "" {
			log.Info("Preserved completed CRIU restore log", "path", copiedPath)
		}
	}

	return notify.restoredPID, cleanup, nil
}

type namedFD struct {
	key  string
	file *os.File
}

type restoreFDLayout struct {
	files []*os.File
	opts  *criurpc.CriuOpts
	fds   map[string]int32
}

func (l *restoreFDLayout) add(key string, file *os.File) {
	fd := l.appendFile(key, file)
	l.opts.InheritFd = append(l.opts.InheritFd, &criurpc.InheritFd{Key: proto.String(key), Fd: proto.Int32(fd)})
}

func (l *restoreFDLayout) appendFile(key string, file *os.File) int32 {
	if l.fds == nil {
		l.fds = make(map[string]int32)
	}
	fd := int32(firstInheritedFD + len(l.files))
	l.files = append(l.files, file)
	l.fds[key] = fd
	return fd
}

func (l *restoreFDLayout) fd(key string) int32 {
	return l.fds[key]
}

func (l *restoreFDLayout) validate() error {
	for _, inherit := range l.opts.GetInheritFd() {
		if inherit.GetFd() != l.fds[inherit.GetKey()] {
			return fmt.Errorf("restore fd layout: key %q is fd %d, want fd %d", inherit.GetKey(), inherit.GetFd(), l.fds[inherit.GetKey()])
		}
	}
	return nil
}

func restoreWithSWRK(binaryPath string, binary *os.File, opts *criurpc.CriuOpts, files []*os.File, nfy criulib.Notify) (retErr error) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	clientFile := os.NewFile(uintptr(fds[0]), "criu-transport-client")
	serverFile := os.NewFile(uintptr(fds[1]), "criu-transport-server")
	conn, err := net.FileConn(clientFile)
	clientFile.Close()
	if err != nil {
		serverFile.Close()
		return err
	}

	extraFiles := append([]*os.File{serverFile}, files...)
	if binary != nil {
		binaryPath = fmt.Sprintf("/proc/self/fd/%d", firstInheritedFD+len(files))
		extraFiles = append(extraFiles, binary)
	}
	cmd := exec.Command(binaryPath, "swrk", strconv.Itoa(swrkTransportFD))
	cmd.ExtraFiles = extraFiles
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		conn.Close()
		serverFile.Close()
		return err
	}
	serverFile.Close()
	defer func() {
		conn.Close()
		if err := cmd.Wait(); err != nil && retErr == nil {
			retErr = fmt.Errorf("criu swrk failed: %w", err)
		}
	}()

	if nfy != nil {
		opts.NotifyScripts = proto.Bool(true)
	}
	restoreType := criurpc.CriuReqType_RESTORE
	request := &criurpc.CriuReq{Type: &restoreType, Opts: opts}
	for {
		data, err := request.MarshalVT()
		if err != nil {
			return err
		}
		if _, err := conn.(*net.UnixConn).Write(data); err != nil {
			return err
		}
		responseData := make([]byte, 2*4096)
		n, err := conn.(*net.UnixConn).Read(responseData)
		if err != nil {
			return err
		}
		response := new(criurpc.CriuResp)
		if err := response.UnmarshalVT(responseData[:n]); err != nil {
			return err
		}
		if !response.GetSuccess() {
			return fmt.Errorf("operation failed (msg:%s err:%d)", response.GetCrErrmsg(), response.GetCrErrno())
		}
		if response.GetType() != criurpc.CriuReqType_NOTIFY {
			return nil
		}
		if nfy == nil {
			return errors.New("unexpected CRIU notify")
		}
		notify := response.GetNotify()
		switch notify.GetScript() {
		case "pre-dump":
			err = nfy.PreDump()
		case "post-dump":
			err = nfy.PostDump()
		case "pre-restore":
			err = nfy.PreRestore()
		case "post-restore":
			err = nfy.PostRestore(notify.GetPid())
		case "network-lock":
			err = nfy.NetworkLock()
		case "network-unlock":
			err = nfy.NetworkUnlock()
		case "setup-namespaces":
			err = nfy.SetupNamespaces(notify.GetPid())
		case "post-setup-namespaces":
			err = nfy.PostSetupNamespaces()
		case "pre-resume":
			if preResume, ok := nfy.(interface{ PreResume() error }); ok {
				err = preResume.PreResume()
			} else {
				err = errors.New("CRIU pre-resume notification is unsupported by the caller")
			}
		case "post-resume":
			err = nfy.PostResume()
		}
		if err != nil {
			notifyType := response.GetType()
			failed := &criurpc.CriuReq{Type: &notifyType, NotifySuccess: proto.Bool(false)}
			data, marshalErr := failed.MarshalVT()
			if marshalErr == nil {
				_, _ = conn.(*net.UnixConn).Write(data)
			}
			return fmt.Errorf("CRIU %s notification failed: %w", notify.GetScript(), err)
		}
		notifyType := response.GetType()
		request = &criurpc.CriuReq{Type: &notifyType, NotifySuccess: proto.Bool(true)}
	}
}

// copyRestoreLog preserves a namespace-local CRIU log where the host-side
// restore agent can read it after nsrestore exits. PageBroker work FDs refer to
// a sidecar-private directory, so prefer the inherited descriptor when set.
func copyRestoreLog(checkpointPath string, workFD int, workDir string) (string, error) {
	return copyRestoreLogTo(checkpointPath, workFD, workDir, RestoreLogFilename+".failed")
}

func copyRestoreLogTo(checkpointPath string, workFD int, workDir, filename string) (string, error) {
	if checkpointPath == "" {
		return "", nil
	}
	workPath := workDir
	if workFD >= 0 {
		workPath = fmt.Sprintf("/proc/self/fd/%d", workFD)
	}
	if workPath == "" {
		return "", nil
	}
	source, err := os.Open(filepath.Join(workPath, RestoreLogFilename))
	if err != nil {
		return "", fmt.Errorf("open %s: %w", RestoreLogFilename, err)
	}
	defer source.Close()

	destination := filepath.Join(checkpointPath, filename)
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", fmt.Errorf("create shared restore log: %w", err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return "", fmt.Errorf("copy restore log: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close shared restore log: %w", closeErr)
	}
	return destination, nil
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
	// An external restore resumes into the placeholder's already-created
	// cgroup. Rebuilding checkpoint cgroups creates CRIU's cgroup yard in the
	// target mount namespace and moves tasks out of that placeholder.
	ignoreCgroups := criurpc.CriuCgMode_IGNORE
	criuOpts.ManageCgroups = proto.Bool(false)
	criuOpts.ManageCgroupsMode = &ignoreCgroups

	// Restore-only options
	criuOpts.RstSibling = proto.Bool(settings.RstSibling)
	criuOpts.MntnsCompatMode = proto.Bool(settings.MntnsCompatMode)
	criuOpts.EvasiveDevices = proto.Bool(settings.EvasiveDevices)
	criuOpts.ForceIrmap = proto.Bool(settings.ForceIrmap)

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

func registerInheritFDs(stdioFDs []string, log logr.Logger) []namedFD {
	if len(stdioFDs) == 0 {
		log.V(1).Info("No stdio FD descriptors in manifest, skipping inherit-fd setup")
		return nil
	}

	var openFiles []namedFD
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
		openFiles = append(openFiles, namedFD{key: target, file: f})
	}

	log.V(1).Info("Registered inherited stdio pipes", "count", len(openFiles))
	return openFiles
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
	preResume   func(int32) error
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

func (n *restoreNotify) PreResume() error {
	n.log.Info("CRIU pre-resume: restored tasks remain frozen", "pid", n.restoredPID)
	if n.preResume == nil {
		return nil
	}
	return n.preResume(n.restoredPID)
}
