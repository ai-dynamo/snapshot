// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fown"
	sk_inet "github.com/checkpoint-restore/go-criu/v8/crit/images/sk-inet"
	sk_opts "github.com/checkpoint-restore/go-criu/v8/crit/images/sk-opts"
	sk_unix "github.com/checkpoint-restore/go-criu/v8/crit/images/sk-unix"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func TestPrepareRestoreImageDirRewritesObservedSocketTopology(t *testing.T) {
	checkpointPath := t.TempDir()
	entries := observedSocketTopology()
	canonical := writeFilesImage(t, checkpointPath, entries)

	mounter := &capturingFilesImageMounter{}
	imageDir, cleanup, err := prepareRestoreImageDirForRestoreIDWithMount(
		checkpointPath,
		987654321,
		t.TempDir(),
		mounter.mount,
	)
	if err != nil {
		t.Fatalf("prepare restore image directory: %v", err)
	}
	if imageDir != checkpointPath {
		t.Fatalf("image directory = %q, want checkpoint %q", imageDir, checkpointPath)
	}

	capturedImages := mounter.capturedImages()
	if len(capturedImages) != 1 {
		t.Fatalf("captured files images = %d, want 1", len(capturedImages))
	}
	restored := decodeFilesImageEntries(t, capturedImages[0])
	if got := restored[0].Usk.Name; !bytes.HasPrefix(got, []byte("\x00dynamo-")) {
		t.Fatalf("CUDA listener address = %q, want Dynamo abstract address", got)
	}
	clientPort := restored[5].Isk.GetSrcPort()
	if clientPort == entries[5].Isk.GetSrcPort() {
		t.Fatalf("TCP client port was not rewritten")
	}
	if got := restored[6].Isk.GetDstPort(); got != clientPort {
		t.Fatalf("TCP server peer port = %d, want %d", got, clientPort)
	}
	dualStackClientPort := restored[9].Isk.GetSrcPort()
	if dualStackClientPort == entries[9].Isk.GetSrcPort() {
		t.Fatalf("dual-stack TCP client port was not rewritten")
	}
	if got := restored[10].Isk.GetDstPort(); got != dualStackClientPort {
		t.Fatalf("dual-stack TCP server peer port = %d, want %d", got, dualStackClientPort)
	}
	if outbound := restored[7].Isk; outbound.GetState() != linuxTCPStateClose ||
		outbound.GetSrcPort() != 0 ||
		outbound.GetDstPort() != 0 {
		t.Fatalf("outbound TCP socket was not disconnected: %v", outbound)
	}
	for i, original := range entries {
		want := proto.Clone(original).(*fdinfo.FileEntry)
		switch i {
		case 0:
			want.Usk.Name = restored[i].Usk.Name
		case 5:
			want.Isk.SrcPort = proto.Uint32(clientPort)
		case 6:
			want.Isk.DstPort = proto.Uint32(clientPort)
		case 7:
			want.Isk.State = proto.Uint32(linuxTCPStateClose)
			want.Isk.SrcPort = proto.Uint32(0)
			want.Isk.DstPort = proto.Uint32(0)
			clear(want.Isk.SrcAddr)
			clear(want.Isk.DstAddr)
		case 9:
			want.Isk.SrcPort = proto.Uint32(dualStackClientPort)
		case 10:
			want.Isk.DstPort = proto.Uint32(dualStackClientPort)
		}
		if !proto.Equal(restored[i], want) {
			t.Errorf("restore entry %d changed unexpectedly:\n got: %v\nwant: %v", i, restored[i], want)
		}
	}

	gotCanonical, err := os.ReadFile(filepath.Join(checkpointPath, filesImageFilename))
	if err != nil {
		t.Fatalf("read canonical image: %v", err)
	}
	if !bytes.Equal(gotCanonical, canonical) {
		t.Fatal("canonical files.img was modified")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup restore image directory: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup restore image directory: %v", err)
	}
	if mounter.cleanupCalls != 1 {
		t.Fatalf("mount cleanup calls = %d, want 1", mounter.cleanupCalls)
	}
}

func TestPrepareRestoreImageDirWithoutRewritesUsesCheckpointDirectly(t *testing.T) {
	checkpointPath := t.TempDir()
	writeFilesImage(t, checkpointPath, []*fdinfo.FileEntry{
		newUnixSocketEntry(1, []byte("/tmp/non-conflicting.sock\x00"), 101, unix.SOCK_STREAM, linuxUnixSocketStateListen),
	})
	mounter := &capturingFilesImageMounter{}

	imageDir, cleanup, err := prepareRestoreImageDirForRestoreIDWithMount(
		checkpointPath,
		987654321,
		t.TempDir(),
		mounter.mount,
	)
	if err != nil {
		t.Fatalf("prepare restore image directory: %v", err)
	}
	if imageDir != checkpointPath {
		t.Fatalf("image directory = %q, want original checkpoint %q", imageDir, checkpointPath)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup no-op image view: %v", err)
	}
	if len(mounter.capturedImages()) != 0 {
		t.Fatalf("mounted replacement images = %d, want 0", len(mounter.capturedImages()))
	}
}

func TestDecodeFilesImageRejectsSymlink(t *testing.T) {
	checkpointPath := t.TempDir()
	realCheckpointPath := t.TempDir()
	writeFilesImage(t, realCheckpointPath, []*fdinfo.FileEntry{
		newUnixSocketEntry(1, []byte("/tmp/socket\x00"), 101, unix.SOCK_STREAM, linuxUnixSocketStateListen),
	})
	if err := os.Symlink(
		filepath.Join(realCheckpointPath, filesImageFilename),
		filepath.Join(checkpointPath, filesImageFilename),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeFilesImage(checkpointPath); err == nil {
		t.Fatal("expected files.img symlink to be rejected")
	}
}

func TestRewriteSocketMetadataRejectsUnexpectedEntryTypes(t *testing.T) {
	image := &crit.CriuImage{
		Entries: []*crit.CriuEntry{{Message: &sk_unix.UnixSkEntry{}}},
	}
	reservationFDs, rewritten, err := rewriteSocketMetadata(image, 987654321)
	if err == nil || !strings.Contains(err.Error(), "unexpected files.img entry 0 type") {
		t.Fatalf("rewrite socket metadata error = %v", err)
	}
	if rewritten || reservationFDs != nil {
		t.Fatalf("rewrite result = (%v, %v), want no reservations or rewrite", reservationFDs, rewritten)
	}
}

func TestPrepareRestoreImageDirConcurrentRewriteMetadataIsIndependent(t *testing.T) {
	checkpointPath := t.TempDir()
	writeFilesImage(t, checkpointPath, observedSocketTopology())

	type result struct {
		path    string
		cleanup func() error
		err     error
	}
	mounter := &capturingFilesImageMounter{}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, restoreID := range []uint64{111, 222} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			path, cleanup, err := prepareRestoreImageDirForRestoreIDWithMount(
				checkpointPath,
				restoreID,
				t.TempDir(),
				mounter.mount,
			)
			results <- result{path: path, cleanup: cleanup, err: err}
		}()
	}
	wait.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			t.Fatalf("prepare restore image directory: %v", result.err)
		}
		cleanup := result.cleanup
		t.Cleanup(func() {
			if err := cleanup(); err != nil {
				t.Errorf("cleanup restore image directory: %v", err)
			}
		})
		if result.path != checkpointPath {
			t.Fatalf("image directory = %q, want checkpoint %q", result.path, checkpointPath)
		}
	}

	addresses := make(map[string]struct{})
	ports := make(map[uint32]struct{})
	for _, capturedImage := range mounter.capturedImages() {
		entries := decodeFilesImageEntries(t, capturedImage)
		addresses[string(entries[0].Usk.Name)] = struct{}{}
		ports[entries[5].Isk.GetSrcPort()] = struct{}{}
	}
	if len(addresses) != 2 {
		t.Fatalf("concurrent restores used %d CUDA socket addresses, want 2", len(addresses))
	}
	if len(ports) != 2 {
		t.Fatalf("concurrent restores used %d TCP client ports, want 2", len(ports))
	}
}

type capturingFilesImageMounter struct {
	mu sync.Mutex

	images       [][]byte
	cleanupCalls int
}

func (m *capturingFilesImageMounter) mount(
	_ string,
	replacementFilesImagePath string,
) (func() error, error) {
	data, err := os.ReadFile(replacementFilesImagePath)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.images = append(m.images, data)
	m.mu.Unlock()
	cleanup := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cleanupCalls++
		return nil
	}
	return cleanup, nil
}

func (m *capturingFilesImageMounter) capturedImages() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.images...)
}

func observedSocketTopology() []*fdinfo.FileEntry {
	wildcard := []uint32{0, 0, 0, 0}
	podAddress := []uint32{0, 0, 0xffff0000, 0x0100007f}
	podIPv4Address := []uint32{0x0100007f}
	remoteIPv4Address := []uint32{0x66d95434}
	listener := newTCPSocketEntry(5, 105, 52103, 0, linuxTCPStateListen, wildcard, wildcard)
	client := newTCPSocketEntry(6, 106, 46730, 52103, linuxTCPStateEstablished, podAddress, podAddress)
	server := newTCPSocketEntry(7, 107, 52103, 46730, linuxTCPStateEstablished, podAddress, podAddress)
	outbound := newTCPSocketEntry(8, 108, 45336, 443, linuxTCPStateEstablished, podIPv4Address, remoteIPv4Address)
	outbound.Isk.Family = proto.Uint32(unix.AF_INET)
	outbound.Isk.V6Only = nil
	dualStackListener := newTCPSocketEntry(9, 109, 53103, 0, linuxTCPStateListen, wildcard, wildcard)
	dualStackClient := newTCPSocketEntry(10, 110, 47730, 53103, linuxTCPStateEstablished, podIPv4Address, podIPv4Address)
	dualStackClient.Isk.Family = proto.Uint32(unix.AF_INET)
	dualStackClient.Isk.V6Only = nil
	dualStackServer := newTCPSocketEntry(11, 111, 53103, 47730, linuxTCPStateEstablished, podAddress, podAddress)
	return []*fdinfo.FileEntry{
		newUnixSocketEntry(1, []byte("\x00cuda-uvmfd-4026554902-1195\x00"), 101, unix.SOCK_SEQPACKET, linuxUnixSocketStateListen),
		newUnixSocketEntry(2, []byte("/tmp/4c85f2c6-ea0e-45cb-b7ee-fd519aba82d0\x00"), 102, unix.SOCK_STREAM, linuxUnixSocketStateListen),
		newUnixSocketEntry(3, []byte("\x00047f9"), 103, unix.SOCK_DGRAM, 7),
		newUnixSocketEntry(4, []byte("\x00047ff"), 104, unix.SOCK_DGRAM, 7),
		listener,
		client,
		server,
		outbound,
		dualStackListener,
		dualStackClient,
		dualStackServer,
	}
}

func newUnixSocketEntry(id uint32, name []byte, inode uint32, socketType int, state uint32) *fdinfo.FileEntry {
	zero32 := uint32(0)
	zero64 := uint64(0)
	return &fdinfo.FileEntry{
		Type: fdinfo.FdTypes_UNIXSK.Enum(),
		Id:   proto.Uint32(id),
		Usk: &sk_unix.UnixSkEntry{
			Id:      proto.Uint32(id),
			Ino:     proto.Uint32(inode),
			Type:    proto.Uint32(uint32(socketType)),
			State:   proto.Uint32(state),
			Flags:   &zero32,
			Uflags:  &zero32,
			Backlog: &zero32,
			Peer:    &zero32,
			Fown: &fown.FownEntry{
				Uid:     &zero32,
				Euid:    &zero32,
				Signum:  &zero32,
				PidType: &zero32,
				Pid:     &zero32,
			},
			Opts: &sk_opts.SkOptsEntry{
				SoSndbuf:     &zero32,
				SoRcvbuf:     &zero32,
				SoSndTmoSec:  &zero64,
				SoSndTmoUsec: &zero64,
				SoRcvTmoSec:  &zero64,
				SoRcvTmoUsec: &zero64,
			},
			Name: name,
			NsId: proto.Uint32(9),
		},
	}
}

func newTCPSocketEntry(
	id, inode, srcPort, dstPort, state uint32,
	srcAddr, dstAddr []uint32,
) *fdinfo.FileEntry {
	socketMetadata := newUnixSocketEntry(id, nil, inode, unix.SOCK_STREAM, state).Usk
	return &fdinfo.FileEntry{
		Type: fdinfo.FdTypes_INETSK.Enum(),
		Id:   proto.Uint32(id),
		Isk: &sk_inet.InetSkEntry{
			Id:      proto.Uint32(id),
			Ino:     proto.Uint32(inode),
			Family:  proto.Uint32(unix.AF_INET6),
			Type:    proto.Uint32(unix.SOCK_STREAM),
			Proto:   proto.Uint32(unix.IPPROTO_TCP),
			State:   proto.Uint32(state),
			SrcPort: proto.Uint32(srcPort),
			DstPort: proto.Uint32(dstPort),
			SrcAddr: srcAddr,
			DstAddr: dstAddr,
			Flags:   socketMetadata.Flags,
			Backlog: socketMetadata.Backlog,
			Fown:    socketMetadata.Fown,
			Opts:    socketMetadata.Opts,
			V6Only:  proto.Bool(false),
			NsId:    proto.Uint32(9),
		},
	}
}

func writeFilesImage(t *testing.T, dir string, entries []*fdinfo.FileEntry) []byte {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, filesImageFilename))
	if err != nil {
		t.Fatalf("create files image: %v", err)
	}
	imageEntries := make([]*crit.CriuEntry, len(entries))
	for i, entry := range entries {
		imageEntries[i] = &crit.CriuEntry{Message: entry}
	}
	image := &crit.CriuImage{
		Magic:     "FILES",
		Entries:   imageEntries,
		EntryType: &fdinfo.FileEntry{},
	}
	if err := crit.New(nil, file, "", false, false).Encode(image); err != nil {
		_ = file.Close()
		t.Fatalf("encode files image: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close files image: %v", err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("read files image: %v", err)
	}
	return data
}

func decodeFilesImageEntries(t *testing.T, data []byte) []*fdinfo.FileEntry {
	t.Helper()
	path := filepath.Join(t.TempDir(), filesImageFilename)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write captured files image: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open captured files image: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close captured files image: %v", err)
		}
	}()
	image, err := crit.New(file, nil, "", false, false).Decode(&fdinfo.FileEntry{})
	if err != nil {
		t.Fatalf("decode files image: %v", err)
	}
	entries := make([]*fdinfo.FileEntry, len(image.Entries))
	for i, entry := range image.Entries {
		entries[i] = entry.Message.(*fdinfo.FileEntry)
	}
	return entries
}
