// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageUsesSourceCheckpointDuringAsyncStaging(t *testing.T) {
	root := t.TempDir()
	checkpoint := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(checkpoint, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\ncheckpoint_id: test\nresident_bytes: 0\nimages: {}\nhost_memory_objects: []\n"
	if err := os.WriteFile(filepath.Join(checkpoint, ManifestFilename), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "scratch")
	staged := filepath.Join(root, "staged")
	if err := os.Mkdir(staged, 0755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "pagebroker.sock")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer connection.Close()
		buffer := make([]byte, 1024)
		_, _ = connection.Read(buffer)
		response := append(append([]byte{0x08, 0x01}, field(3, []byte(staged))...), field(4, []byte(scratch))...)
		_, _ = connection.Write(response)
		_, _ = connection.Write([]byte("provider-ready"))
	}()

	transaction, err := Stage(context.Background(), socket, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.StagingPath() != checkpoint {
		t.Fatalf("restore path = %q, want complete source checkpoint %q", transaction.StagingPath(), checkpoint)
	}
	files, err := transaction.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("inherited files = %d, want image, work, provider, control", len(files))
	}
	providerMessage := make([]byte, 32)
	n, err := files[2].Read(providerMessage)
	if err != nil {
		t.Fatal(err)
	}
	if string(providerMessage[:n]) != "provider-ready" {
		t.Fatalf("provider message = %q", providerMessage[:n])
	}
	for _, file := range files {
		_ = file.Close()
	}
}

func TestWaitReadyBlocksAndPropagatesBackendFailure(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "pagebroker.sock")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	release := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		buffer := make([]byte, 1024)
		_, _ = connection.Read(buffer)
		<-release
		_, _ = connection.Write(field(5, []byte("read manifest source range")))
	}()

	control, err := connectFile(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	result := make(chan error, 1)
	go func() { result <- WaitReady(control, "tx-test") }()
	select {
	case err := <-result:
		t.Fatalf("WaitReady returned before materialization completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "read manifest source range") {
			t.Fatalf("WaitReady error = %v, want backend read failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitReady did not release after backend completion")
	}
}
