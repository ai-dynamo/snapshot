// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	sk_inet "github.com/checkpoint-restore/go-criu/v8/crit/images/sk-inet"
	sk_unix "github.com/checkpoint-restore/go-criu/v8/crit/images/sk-unix"
	"golang.org/x/sys/unix"
)

const (
	filesImageFilename            = "files.img"
	placeholderMountNamespacePath = "/proc/self/ns/mnt"
	cudaUVMFDSocketNamePrefix     = "\x00cuda-uvmfd-"
	linuxUnixSocketStateListen    = 10
	linuxTCPStateEstablished      = 1
	linuxTCPStateClose            = 7
	linuxTCPStateListen           = 10
)

type tcpPortRewrite struct {
	socket *sk_inet.InetSkEntry
	peers  []*sk_inet.InetSkEntry
	port   uint32
}

type mountFilesImageFunc func(checkpointPath, replacementFilesImagePath string) (func() error, error)

func prepareRestoreImageDir(checkpointPath, scratchDir string) (string, func() error, error) {
	// The placeholder mount namespace remains container-specific with shareProcessNamespace.
	var stat unix.Stat_t
	if err := unix.Stat(placeholderMountNamespacePath, &stat); err != nil {
		return "", nil, fmt.Errorf("failed to stat placeholder mount namespace at %s: %w", placeholderMountNamespacePath, err)
	}
	return prepareRestoreImageDirForRestoreID(checkpointPath, stat.Ino, scratchDir)
}

func prepareRestoreImageDirForRestoreID(checkpointPath string, restoreID uint64, scratchDir string) (string, func() error, error) {
	return prepareRestoreImageDirForRestoreIDWithMount(
		checkpointPath,
		restoreID,
		scratchDir,
		mountFilesImage,
	)
}

func prepareRestoreImageDirForRestoreIDWithMount(
	checkpointPath string,
	restoreID uint64,
	scratchDir string,
	mountReplacement mountFilesImageFunc,
) (string, func() error, error) {
	checkpointPath, err := filepath.Abs(checkpointPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve checkpoint path: %w", err)
	}

	image, err := decodeFilesImage(checkpointPath)
	if err != nil {
		return "", nil, err
	}
	reservationFDs, rewritten, err := rewriteSocketMetadata(image, restoreID)
	if err != nil {
		return "", nil, err
	}
	if !rewritten {
		closeFDs(reservationFDs)
		return checkpointPath, func() error { return nil }, nil
	}

	replacementFilesImagePath, err := encodeReplacementFilesImage(image, scratchDir)
	if err != nil {
		closeFDs(reservationFDs)
		return "", nil, err
	}
	removeImageView, err := mountReplacementFilesImage(checkpointPath, replacementFilesImagePath, mountReplacement)
	if err != nil {
		closeFDs(reservationFDs)
		return "", nil, err
	}
	return checkpointPath, restoreImageCleanup(reservationFDs, removeImageView), nil
}

func decodeFilesImage(checkpointPath string) (*crit.CriuImage, error) {
	path := filepath.Join(checkpointPath, filesImageFilename)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", filesImageFilename, err)
	}
	filesImage := os.NewFile(uintptr(fd), path)
	info, err := filesImage.Stat()
	if err != nil {
		_ = filesImage.Close()
		return nil, fmt.Errorf("failed to stat %s: %w", filesImageFilename, err)
	}
	if !info.Mode().IsRegular() {
		_ = filesImage.Close()
		return nil, fmt.Errorf("%s is not a regular file", filesImageFilename)
	}
	image, decodeErr := crit.New(filesImage, nil, "", false, false).Decode(&fdinfo.FileEntry{})
	closeErr := filesImage.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("failed to decode %s: %w", filesImageFilename, decodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close %s: %w", filesImageFilename, closeErr)
	}
	return image, nil
}

// rewriteSocketMetadata gives each clone private socket identities while
// preserving listeners. The caller owns the returned reservation descriptors.
func rewriteSocketMetadata(image *crit.CriuImage, restoreID uint64) ([]int, bool, error) {
	tcpRewrites, tcpDisconnects, forbiddenPorts := planTCPPortRewrites(image)
	var reservationFDs []int
	for i := range tcpRewrites {
		port, reservationFD, err := reserveDualStackTCPPort(forbiddenPorts)
		if err != nil {
			closeFDs(reservationFDs)
			return nil, false, fmt.Errorf("failed to reserve replacement TCP port: %w", err)
		}
		tcpRewrites[i].port = port
		reservationFDs = append(reservationFDs, reservationFD)
		forbiddenPorts[port] = struct{}{}
	}

	rewritten := false
	for i, entry := range image.Entries {
		fileEntry, ok := entry.Message.(*fdinfo.FileEntry)
		if !ok {
			closeFDs(reservationFDs)
			return nil, false, fmt.Errorf("unexpected %s entry %d type %T", filesImageFilename, i, entry.Message)
		}
		if fileEntry.GetType() == fdinfo.FdTypes_UNIXSK && fileEntry.Usk != nil &&
			rewriteCloneConflictingUnixSocketAddress(fileEntry.Usk, restoreID) {
			rewritten = true
		}
	}
	for _, rewrite := range tcpRewrites {
		*rewrite.socket.SrcPort = rewrite.port
		for _, peer := range rewrite.peers {
			*peer.DstPort = rewrite.port
		}
		rewritten = true
	}
	for _, socket := range tcpDisconnects {
		// A remote peer cannot follow a cloned connection to its new tuple.
		// Preserve the FD, but restore it as an unconnected TCP socket.
		*socket.State = linuxTCPStateClose
		*socket.SrcPort = 0
		*socket.DstPort = 0
		clear(socket.SrcAddr)
		clear(socket.DstAddr)
		rewritten = true
	}
	return reservationFDs, rewritten, nil
}

func encodeReplacementFilesImage(image *crit.CriuImage, scratchDir string) (string, error) {
	filesImage, err := os.CreateTemp(scratchDir, ".dynamo-criu-files-*.img")
	if err != nil {
		return "", fmt.Errorf("failed to create private %s: %w", filesImageFilename, err)
	}
	path := filesImage.Name()
	if err := crit.New(nil, filesImage, "", false, false).Encode(image); err != nil {
		_ = filesImage.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("failed to encode private %s: %w", filesImageFilename, err)
	}
	if err := filesImage.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("failed to close private %s: %w", filesImageFilename, err)
	}
	return path, nil
}

func mountReplacementFilesImage(
	checkpointPath string,
	replacementFilesImagePath string,
	mountReplacement mountFilesImageFunc,
) (func() error, error) {
	removeImageView, err := mountReplacement(checkpointPath, replacementFilesImagePath)
	if err != nil {
		_ = os.Remove(replacementFilesImagePath)
		return nil, fmt.Errorf("failed to mount private %s: %w", filesImageFilename, err)
	}
	if err := os.Remove(replacementFilesImagePath); err != nil {
		cleanupErr := removeImageView()
		if cleanupErr != nil {
			return nil, fmt.Errorf("failed to unlink mounted private %s: %w (cleanup failed: %v)", filesImageFilename, err, cleanupErr)
		}
		return nil, fmt.Errorf("failed to unlink mounted private %s: %w", filesImageFilename, err)
	}
	return removeImageView, nil
}

func restoreImageCleanup(reservationFDs []int, removeImageView func() error) func() error {
	var once sync.Once
	var cleanupErr error
	return func() error {
		once.Do(func() {
			closeFDs(reservationFDs)
			cleanupErr = removeImageView()
		})
		return cleanupErr
	}
}

func planTCPPortRewrites(image *crit.CriuImage) (
	[]tcpPortRewrite,
	[]*sk_inet.InetSkEntry,
	map[uint32]struct{},
) {
	var sockets []*sk_inet.InetSkEntry
	forbiddenPorts := make(map[uint32]struct{})

	for _, entry := range image.Entries {
		file, ok := entry.Message.(*fdinfo.FileEntry)
		if !ok || file.GetType() != fdinfo.FdTypes_INETSK || file.Isk == nil {
			continue
		}
		sockets = append(sockets, file.Isk)
		if port := file.Isk.GetSrcPort(); port != 0 {
			forbiddenPorts[port] = struct{}{}
		}
		if port := file.Isk.GetDstPort(); port != 0 {
			forbiddenPorts[port] = struct{}{}
		}
	}

	var rewrites []tcpPortRewrite
	var disconnects []*sk_inet.InetSkEntry
	// Keep listener ports stable. Only rewrite connections whose reciprocal
	// endpoint is also in the image, so both halves retain a valid TCP tuple.
	for _, socket := range sockets {
		if !isEstablishedTCP(socket) || !hasSupportedTCPAddresses(socket) {
			continue
		}
		rewrite := tcpPortRewrite{socket: socket}
		for _, peer := range sockets {
			if reciprocalTCPPair(socket, peer) {
				rewrite.peers = append(rewrite.peers, peer)
			}
		}
		if len(rewrite.peers) == 0 {
			disconnects = append(disconnects, socket)
		} else if len(rewrite.peers) == 1 && !hasTCPListener(socket, sockets) {
			rewrites = append(rewrites, rewrite)
		}
	}
	return rewrites, disconnects, forbiddenPorts
}

func isTCPSocket(socket *sk_inet.InetSkEntry) bool {
	return socket != nil &&
		socket.SrcPort != nil &&
		socket.DstPort != nil &&
		socket.NsId != nil &&
		(socket.GetFamily() == uint32(unix.AF_INET) ||
			socket.GetFamily() == uint32(unix.AF_INET6)) &&
		socket.GetType() == uint32(unix.SOCK_STREAM) &&
		socket.GetProto() == uint32(unix.IPPROTO_TCP)
}

func isEstablishedTCP(socket *sk_inet.InetSkEntry) bool {
	return isTCPSocket(socket) &&
		socket.GetState() == linuxTCPStateEstablished &&
		socket.GetSrcPort() > 0 &&
		socket.GetDstPort() > 0
}

func reciprocalTCPPair(a, b *sk_inet.InetSkEntry) bool {
	aSrc, aSrcOK := normalizedIPAddress(a.GetFamily(), a.SrcAddr)
	aDst, aDstOK := normalizedIPAddress(a.GetFamily(), a.DstAddr)
	bSrc, bSrcOK := normalizedIPAddress(b.GetFamily(), b.SrcAddr)
	bDst, bDstOK := normalizedIPAddress(b.GetFamily(), b.DstAddr)
	return a != b &&
		isEstablishedTCP(a) &&
		isEstablishedTCP(b) &&
		aSrcOK && aDstOK && bSrcOK && bDstOK &&
		a.GetNsId() == b.GetNsId() &&
		a.GetSrcPort() == b.GetDstPort() &&
		a.GetDstPort() == b.GetSrcPort() &&
		aSrc == bDst &&
		aDst == bSrc
}

func hasSupportedTCPAddresses(socket *sk_inet.InetSkEntry) bool {
	_, srcOK := normalizedIPAddress(socket.GetFamily(), socket.SrcAddr)
	_, dstOK := normalizedIPAddress(socket.GetFamily(), socket.DstAddr)
	return srcOK && dstOK
}

func normalizedIPAddress(family uint32, words []uint32) (netip.Addr, bool) {
	switch family {
	case unix.AF_INET:
		if len(words) != 1 {
			return netip.Addr{}, false
		}
		var address [4]byte
		binary.LittleEndian.PutUint32(address[:], words[0])
		return netip.AddrFrom4(address), true
	case unix.AF_INET6:
		if len(words) != 4 {
			return netip.Addr{}, false
		}
		var address [16]byte
		for i, word := range words {
			binary.LittleEndian.PutUint32(address[i*4:], word)
		}
		return netip.AddrFrom16(address).Unmap(), true
	default:
		return netip.Addr{}, false
	}
}

func hasTCPListener(
	endpoint *sk_inet.InetSkEntry,
	sockets []*sk_inet.InetSkEntry,
) bool {
	for _, socket := range sockets {
		if !isTCPSocket(socket) ||
			socket.GetState() != linuxTCPStateListen ||
			socket.GetFamily() != endpoint.GetFamily() ||
			socket.GetNsId() != endpoint.GetNsId() ||
			socket.GetSrcPort() != endpoint.GetSrcPort() ||
			socket.GetDstPort() != 0 {
			continue
		}
		return true
	}
	return false
}

func reserveDualStackTCPPort(forbidden map[uint32]struct{}) (uint32, int, error) {
	var rejected []int
	defer func() {
		for _, fd := range rejected {
			_ = unix.Close(fd)
		}
	}()

	for {
		fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
		if err != nil {
			return 0, -1, err
		}
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			_ = unix.Close(fd)
			return 0, -1, err
		}
		if err := unix.Bind(fd, &unix.SockaddrInet6{}); err != nil {
			_ = unix.Close(fd)
			return 0, -1, err
		}

		boundAddress, err := unix.Getsockname(fd)
		if err != nil {
			_ = unix.Close(fd)
			return 0, -1, err
		}
		address, ok := boundAddress.(*unix.SockaddrInet6)
		if !ok {
			_ = unix.Close(fd)
			return 0, -1, fmt.Errorf("unexpected bound socket address %T", boundAddress)
		}
		port := uint32(address.Port)
		if port == 0 || port > 65535 {
			_ = unix.Close(fd)
			return 0, -1, fmt.Errorf("kernel selected invalid TCP port %d", port)
		}
		if _, exists := forbidden[port]; exists {
			// Keep the bind until selection finishes so port 0 cannot immediately
			// return the same forbidden port.
			rejected = append(rejected, fd)
			continue
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			_ = unix.Close(fd)
			return 0, -1, err
		}
		return port, fd, nil
	}
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}

func rewriteCloneConflictingUnixSocketAddress(entry *sk_unix.UnixSkEntry, restoreID uint64) bool {
	if !isCUDAUVMFDListener(entry) {
		return false
	}

	// CUDA retains this listener's FD, so only its clone-private address changes.
	input := make([]byte, 8+len(entry.Name))
	binary.BigEndian.PutUint64(input, restoreID)
	copy(input[8:], entry.Name)
	digest := sha256.Sum256(input)
	entry.Name = hex.AppendEncode([]byte("\x00dynamo-"), digest[:])
	return true
}

func isCUDAUVMFDListener(entry *sk_unix.UnixSkEntry) bool {
	return entry != nil &&
		entry.Type != nil &&
		entry.State != nil &&
		entry.Peer != nil &&
		*entry.Type == uint32(unix.SOCK_SEQPACKET) &&
		*entry.State == linuxUnixSocketStateListen &&
		*entry.Peer == 0 &&
		bytes.HasPrefix(entry.Name, []byte(cudaUVMFDSocketNamePrefix))
}
