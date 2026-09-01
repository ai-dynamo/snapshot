// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDetectCUDAInterpositionSkipsEmptyPIDList(t *testing.T) {
	interposed, err := DetectCUDAInterposition(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("DetectCUDAInterposition() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCUDAInterposition() = true, want skip")
	}
}

func TestDetectCUDAInterpositionPIDCountMismatch(t *testing.T) {
	_, err := DetectCUDAInterposition(t.TempDir(), []int{101}, nil)
	if err == nil {
		t.Fatal("expected PID mapping count mismatch")
	}
}

func TestDetectCUDAInterpositionSkipsWithoutSockets(t *testing.T) {
	procRoot := shortTempDir(t)
	mustControlDir(t, procRoot, 101, 1)
	interposed, err := DetectCUDAInterposition(procRoot, []int{101, 102}, []int{1, 2})
	if err != nil {
		t.Fatalf("DetectCUDAInterposition() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCUDAInterposition() = true, want skip")
	}
}

func TestDetectCUDAInterpositionIgnoresProcfsEnviron(t *testing.T) {
	procRoot := shortTempDir(t)
	mustControlDir(t, procRoot, 101, 1)
	mustControlDir(t, procRoot, 102, 2)
	mustEnviron(t, procRoot, 101, "IRRELEVANT=1\x00")
	mustEnviron(t, procRoot, 102, "IRRELEVANT=1\x00")

	interposed, err := DetectCUDAInterposition(procRoot, []int{101, 102}, []int{1, 2})
	if err != nil {
		t.Fatalf("DetectCUDAInterposition() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCUDAInterposition() keyed off procfs environ")
	}
}

func TestDetectCUDAInterpositionRequiresEveryCUDAProcessSocket(t *testing.T) {
	procRoot := shortTempDir(t)
	listenUnix(t, cuinterposerEndpointPath(procRoot, 101, 1))

	_, err := DetectCUDAInterposition(procRoot, []int{101, 102}, []int{1, 2})
	if err == nil {
		t.Fatal("expected missing endpoint to fail closed")
	}

	listenUnix(t, cuinterposerEndpointPath(procRoot, 102, 2))
	interposed, err := DetectCUDAInterposition(procRoot, []int{101, 102}, []int{1, 2})
	if err != nil {
		t.Fatalf("DetectCUDAInterposition() error = %v", err)
	}
	if !interposed {
		t.Fatal("DetectCUDAInterposition() = false, want true")
	}
}

func TestDetectCUDAInterpositionRejectsNonSocketEndpoint(t *testing.T) {
	procRoot := shortTempDir(t)
	listenUnix(t, cuinterposerEndpointPath(procRoot, 101, 1))
	path := cuinterposerEndpointPath(procRoot, 102, 2)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := DetectCUDAInterposition(procRoot, []int{101, 102}, []int{1, 2})
	if err == nil {
		t.Fatal("expected non-socket endpoint to fail closed")
	}
}

func TestHasCUDAInterpositionState(t *testing.T) {
	checkpointDir := t.TempDir()
	present, err := HasCUDAInterpositionState(checkpointDir)
	if err != nil {
		t.Fatalf("HasCUDAInterpositionState() error = %v", err)
	}
	if present {
		t.Fatal("HasCUDAInterpositionState() = true for missing sidecar")
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, cuinterposerStateFile), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	present, err = HasCUDAInterpositionState(checkpointDir)
	if err != nil {
		t.Fatalf("HasCUDAInterpositionState() error = %v", err)
	}
	if !present {
		t.Fatal("HasCUDAInterpositionState() = false, want true")
	}
}

func TestOpenCUDAInterpositionBroker(t *testing.T) {
	socketDir := shortTempDir(t)
	socketPath := filepath.Join(socketDir, "test-"+strconv.Itoa(os.Getpid())+".sock")
	_ = os.Remove(socketPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	accepted := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			acceptErr = connection.Close()
		}
		accepted <- acceptErr
	}()

	checkpointDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(checkpointDir, cuinterposerBrokerFile),
		[]byte(socketPath+"\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	broker, err := openCUDAInterpositionBroker(checkpointDir, socketDir)
	if err != nil {
		t.Fatalf("OpenCUDAInterpositionBroker() error = %v", err)
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestOpenCUDAInterpositionBrokerRejectsInvalidState(t *testing.T) {
	for _, content := range []string{
		"",
		"/tmp/not-the-broker.sock\n",
		cuinterposerBrokerDir + "/nested/broker.sock\n",
		cuinterposerBrokerDir + "/broker.sock\ntrailing\n",
		cuinterposerBrokerDir + "/broker.sock",
	} {
		checkpointDir := t.TempDir()
		if err := os.WriteFile(
			filepath.Join(checkpointDir, cuinterposerBrokerFile),
			[]byte(content),
			0600,
		); err != nil {
			t.Fatal(err)
		}
		if broker, err := OpenCUDAInterpositionBroker(checkpointDir); err == nil {
			_ = broker.Close()
			t.Fatalf("OpenCUDAInterpositionBroker(%q) succeeded", content)
		}
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mustControlDir(t *testing.T, procRoot string, observedPID, namespacePID int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(cuinterposerEndpointPath(procRoot, observedPID, namespacePID)), 0700); err != nil {
		t.Fatal(err)
	}
}

func mustEnviron(t *testing.T, procRoot string, observedPID int, content string) {
	t.Helper()
	path := filepath.Join(procRoot, strconv.Itoa(observedPID), "environ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}
